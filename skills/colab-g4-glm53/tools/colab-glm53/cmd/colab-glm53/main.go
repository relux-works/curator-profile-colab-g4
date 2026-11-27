// Command colab-glm53 manages Google Colab G4 (RTX PRO 6000 Blackwell 96 GB)
// instances serving GLM-5.3-Flash Uncensored via Project Maya over an SSH
// tunnel to http://127.0.0.1:<port>/v1.
//
// Stdlib-only Go so it compiles under Curator's closed go-v1 driver on both
// macOS and Linux (Ubuntu) with zero external Go dependencies.
package main

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

//go:embed remote_provision.sh
var remoteProvisionScript string

const (
	defaultProfile    = "colab-g4-keeper"
	defaultSession    = "glm53"
	defaultPort       = 18100
	defaultRemotePort = 8090
	defaultTier       = "2.51"
	defaultCtx        = 131072
)

type tierSpec struct {
	HFRepo   string
	ModelDir string
	SpecMinP string
}

var tiers = map[string]tierSpec{
	"2.51": {
		HFRepo:   "MikeRoz/GLM-5.3-Flash-Uncensored-2.51bpw-h6-exl3",
		ModelDir: "MikeRoz-2.51bpw-uncensored",
		SpecMinP: "0.95",
	},
	"3.05": {
		HFRepo:   "MikeRoz/GLM-5.3-Flash-Uncensored-3.05bpw-h6-exl3",
		ModelDir: "MikeRoz-3.05bpw-uncensored",
		SpecMinP: "0.90",
	},
	"4.05": {
		HFRepo:   "MikeRoz/GLM-5.3-Flash-Uncensored-4.05bpw-h6-exl3",
		ModelDir: "MikeRoz-4.05bpw-uncensored",
		SpecMinP: "0.95",
	},
}

type authStatusReport struct {
	Authenticated  bool   `json:"authenticated"`
	SandboxBlocked bool   `json:"sandbox_blocked,omitempty"`
	ColabBin       string `json:"colab_bin,omitempty"`
	Platform       string `json:"platform"`
	Reason         string `json:"reason,omitempty"`
	Hint           string `json:"hint,omitempty"`
	UsageSummary   string `json:"usage_summary,omitempty"`
	LoginCommand   string `json:"login_command,omitempty"`
}

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		printUsage()
		return 0
	}
	sub := args[0]
	rest := args[1:]
	switch sub {
	case "auth-status":
		return cmdAuthStatus(rest)
	case "doctor":
		return cmdDoctor(rest)
	case "usage":
		return cmdColabPassthrough("usage", rest)
	case "sessions":
		return cmdColabPassthrough("sessions", rest)
	case "status":
		return cmdStatus(rest)
	case "start":
		return cmdStart(rest)
	case "tunnel":
		return cmdTunnel(rest)
	case "stop":
		return cmdStop(rest)
	default:
		fmt.Fprintf(os.Stderr, "colab-glm53: unknown subcommand %q\n\n", sub)
		printUsage()
		return 2
	}
}

func printUsage() {
	fmt.Print(`colab-glm53 — Google Colab G4 (RTX PRO 6000 96 GB) launcher for GLM-5.3-Flash

Usage:
  colab-glm53 auth-status [--json] [--profile <name>]
  colab-glm53 doctor [--json] [--profile <name>]
  colab-glm53 usage
  colab-glm53 sessions
  colab-glm53 status [--session glm53] [--port 18100]
  colab-glm53 start [--tier 2.51|3.05|4.05] [--ctx 131072|262144] [--port 18100] [--session glm53] [--bg] [--no-wait]
  colab-glm53 tunnel [--session glm53] [--port 18100] [--bg]
  colab-glm53 stop [--session glm53] [--port 18100]
`)
}

func resolveColabBin() (string, error) {
	if envBin := strings.TrimSpace(os.Getenv("COLAB_BIN")); envBin != "" {
		if info, err := os.Stat(envBin); err == nil && !info.IsDir() {
			return envBin, nil
		}
		return "", fmt.Errorf("COLAB_BIN=%q does not exist", envBin)
	}
	if p, err := exec.LookPath("colab"); err == nil {
		return p, nil
	}
	if home, err := os.UserHomeDir(); err == nil {
		candidate := filepath.Join(home, ".local", "bin", "colab")
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate, nil
		}
	}
	return "", errors.New("colab CLI not found on PATH or at ~/.local/bin/colab")
}

func resolveSSHKey() string {
	if k := strings.TrimSpace(os.Getenv("COLAB_SSH_KEY")); k != "" {
		return k
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".ssh", "id_ed25519_colab")
}

func ensureSSHKey(keyPath string) error {
	if _, err := os.Stat(keyPath); err == nil {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(keyPath), 0o700); err != nil {
		return err
	}
	fmt.Printf("[colab-glm53] Generating ed25519 SSH key at %s ...\n", keyPath)
	cmd := exec.Command("ssh-keygen", "-t", "ed25519", "-N", "", "-C", "colab-cli", "-f", keyPath)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// checkAuthStatus probes Colab OAuth2 readiness non-interactively (never hangs
// waiting on verification code input and never reads or prints secret bytes).
func checkAuthStatus(profile string) authStatusReport {
	if profile == "" {
		profile = defaultProfile
	}
	loginCmd := fmt.Sprintf("curator profile exec %s -- colab usage", profile)
	rep := authStatusReport{
		Platform:     runtime.GOOS + "-" + runtime.GOARCH,
		LoginCommand: loginCmd,
	}
	colabBin, err := resolveColabBin()
	if err != nil {
		rep.Reason = "colab_cli_missing"
		return rep
	}
	rep.ColabBin = colabBin

	// On macOS, check Keychain item existence WITHOUT -w (no secret material read)
	// before invoking colab so an unauthenticated host returns instantaneously
	// without spawning Python or hitting Google OAuth endpoints.
	// Distinguish a genuinely missing Keychain item from a restricted agent
	// sandbox (e.g., Codex Seatbelt workspace-write) that blocks SecurityServer
	// (SecKeychainSearchCreateFromAttributes / errSecParam -50).
	if runtime.GOOS == "darwin" {
		home, _ := os.UserHomeDir()
		legacyToken := filepath.Join(home, ".config", "colab-cli", "token.json")
		_, legacyErr := os.Stat(legacyToken)
		kcOutBytes, kcErr := exec.Command("security", "find-generic-password", "-s", "google-colab-cli", "-a", "oauth_token").CombinedOutput()
		if kcErr != nil && legacyErr != nil {
			kcLower := strings.ToLower(string(kcOutBytes))
			if strings.Contains(kcLower, "seckeychainsearchcreatefromattributes") ||
				strings.Contains(kcLower, "one or more parameters passed") ||
				strings.Contains(kcLower, "operation not permitted") ||
				strings.Contains(kcLower, "user interaction is not allowed") ||
				strings.Contains(kcLower, "(-50") {
				rep.SandboxBlocked = true
				rep.Reason = "sandbox_keychain_blocked"
				rep.LoginCommand = ""
				rep.Hint = "Command ran inside a restricted agent sandbox that blocks macOS Keychain (SecurityServer) and/or outbound network. Re-run with escalated/unsandboxed execution, or launch the agent with --yolo."
				return rep
			}
			rep.Reason = "missing_oauth_token"
			return rep
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, colabBin, "usage")
	cmd.Stdin = bytes.NewReader(nil) // closed stdin guarantees non-interactive exit
	outBytes, runErr := cmd.CombinedOutput()
	out := strings.TrimSpace(string(outBytes))
	lower := strings.ToLower(out)

	if strings.Contains(lower, "could not read the oauth token from the os keyring") ||
		strings.Contains(lower, "can't get password from keychain") ||
		strings.Contains(lower, "(-50") ||
		strings.Contains(lower, "operation not permitted") ||
		strings.Contains(lower, "nodename nor servname") ||
		strings.Contains(lower, "temporary failure in name resolution") ||
		strings.Contains(lower, "failed to establish a new connection") {
		rep.SandboxBlocked = true
		rep.Reason = "sandbox_keychain_or_network_blocked"
		rep.LoginCommand = ""
		rep.Hint = "Command ran inside a restricted agent sandbox that blocks OS keyring and/or outbound network. Re-run with escalated/unsandboxed execution, or launch the agent with --yolo."
		return rep
	}

	if strings.Contains(lower, "applicationdefaultauthcode") ||
		strings.Contains(lower, "enter the authorization code") ||
		strings.Contains(lower, "to authorize colab-cli") ||
		strings.Contains(lower, "enter verification code") ||
		strings.Contains(lower, "accounts.google.com/o/oauth2") ||
		strings.Contains(lower, "eoferror") ||
		strings.Contains(lower, "invalid_grant") ||
		strings.Contains(lower, "unauthorized") ||
		strings.Contains(lower, "token has been expired") {
		rep.Reason = "oauth_required_or_expired"
		return rep
	}
	if runErr != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			rep.Reason = "usage_check_timed_out"
		} else {
			rep.Reason = "usage_check_failed: " + firstNonEmptyLine(out)
		}
		return rep
	}
	rep.Authenticated = true
	rep.UsageSummary = out
	rep.LoginCommand = ""
	return rep
}

func firstNonEmptyLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			if len(line) > 200 {
				return line[:200]
			}
			return line
		}
	}
	return ""
}

func cmdAuthStatus(args []string) int {
	fs := flag.NewFlagSet("auth-status", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "output JSON")
	profile := fs.String("profile", defaultProfile, "profile name for login command hint")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	rep := checkAuthStatus(*profile)
	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(rep)
	} else if rep.Authenticated {
		fmt.Printf("Authenticated: yes (colab=%s)\n%s\n", rep.ColabBin, rep.UsageSummary)
	} else if rep.SandboxBlocked {
		fmt.Printf("Authenticated: unknown (%s)\n%s\n", rep.Reason, rep.Hint)
	} else {
		fmt.Printf("Authenticated: no (%s)\nRun this command in a separate terminal window to sign in with your Google AI Ultra account:\n  %s\n", rep.Reason, rep.LoginCommand)
	}
	if !rep.Authenticated {
		return 1
	}
	return 0
}

func cmdDoctor(args []string) int {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "output JSON")
	profile := fs.String("profile", defaultProfile, "profile name")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	auth := checkAuthStatus(*profile)
	bins := map[string]string{}
	for _, name := range []string{"colab", "ssh", "ssh-keygen", "curl"} {
		if p, err := exec.LookPath(name); err == nil {
			bins[name] = p
		} else {
			bins[name] = ""
		}
	}
	if auth.ColabBin != "" {
		bins["colab"] = auth.ColabBin
	}
	keyPath := resolveSSHKey()
	_, keyErr := os.Stat(keyPath)
	report := map[string]any{
		"platform":    runtime.GOOS + "-" + runtime.GOARCH,
		"binaries":    bins,
		"ssh_key":     keyPath,
		"ssh_key_ok":  keyErr == nil,
		"auth_status": auth,
	}
	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(report)
	} else {
		fmt.Printf("Platform: %s\n", report["platform"])
		for _, name := range []string{"colab", "ssh", "ssh-keygen", "curl"} {
			val := bins[name]
			if val == "" {
				val = "MISSING"
			}
			fmt.Printf("  %-12s %s\n", name+":", val)
		}
		fmt.Printf("  %-12s %s (exists=%v)\n", "ssh_key:", keyPath, keyErr == nil)
		fmt.Printf("  %-12s %v\n", "auth:", auth.Authenticated)
		if !auth.Authenticated {
			fmt.Printf("  login cmd:   %s\n", auth.LoginCommand)
		}
	}
	if !auth.Authenticated || bins["colab"] == "" || bins["ssh"] == "" || bins["ssh-keygen"] == "" {
		return 1
	}
	return 0
}

func cmdColabPassthrough(sub string, args []string) int {
	auth := checkAuthStatus(defaultProfile)
	if !auth.Authenticated {
		fmt.Fprintf(os.Stderr, "[colab-glm53] Not authenticated (%s).\nRun in a separate terminal:\n  %s\n", auth.Reason, auth.LoginCommand)
		return 1
	}
	cmdArgs := append([]string{sub}, args...)
	cmd := exec.Command(auth.ColabBin, cmdArgs...)
	cmd.Stdin = bytes.NewReader(nil)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return 1
	}
	return 0
}

func cmdStatus(args []string) int {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	session := fs.String("session", defaultSession, "Colab session name")
	port := fs.Int("port", defaultPort, "local tunnel port")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	auth := checkAuthStatus(defaultProfile)
	if !auth.Authenticated {
		fmt.Fprintf(os.Stderr, "[colab-glm53] Not authenticated (%s).\nRun in a separate terminal:\n  %s\n", auth.Reason, auth.LoginCommand)
		return 1
	}
	fmt.Println("=== Colab session status ===")
	c1 := exec.Command(auth.ColabBin, "status", "-s", *session)
	c1.Stdin = bytes.NewReader(nil)
	c1.Stdout = os.Stdout
	c1.Stderr = os.Stderr
	_ = c1.Run()

	fmt.Println("\n=== Compute unit usage ===")
	fmt.Println(auth.UsageSummary)

	fmt.Printf("\n=== Local tunnel (http://127.0.0.1:%d/v1/models) ===\n", *port)
	if body, ok := probeModels(*port); ok {
		fmt.Printf("ONLINE (200 OK): %s\n", strings.TrimSpace(body))
	} else {
		fmt.Println("OFFLINE (no local tunnel or server not ready)")
	}
	return 0
}

var (
	hardStopRe = regexp.MustCompile(`(?i)Allocation refused|precondition failed|Backend rejected|TooManyAssignmentsError|Too Many Requests|Unauthorized|Forbidden|not entitled|no quota|insufficient|invalid_grant|token.*expired|HTTP[^0-9]*(401|403|429)`)
	retryRe    = regexp.MustCompile(`(?i)Service\s*Unavailable|503|502|504|ReadTimeout|timed out|Temporary|capacity|Resource\s*Exhausted`)
	boxLineRe  = regexp.MustCompile(`(?m)^[╭│╰].*$`)
)

func cmdStart(args []string) int {
	fs := flag.NewFlagSet("start", flag.ContinueOnError)
	tier := fs.String("tier", defaultTier, "EXL3 quant tier: 2.51, 3.05, or 4.05")
	ctxLen := fs.Int("ctx", defaultCtx, "context length: 131072 or 262144")
	port := fs.Int("port", defaultPort, "local port for SSH tunnel")
	session := fs.String("session", defaultSession, "Colab session name")
	bg := fs.Bool("bg", false, "run SSH tunnel in background")
	noWait := fs.Bool("no-wait", false, "do not retry on 503 capacity errors")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	// Accept positional tier (e.g. `colab-glm53 start 3.05`) for parity with start.sh.
	if fs.NArg() == 1 {
		*tier = fs.Arg(0)
	} else if fs.NArg() > 1 {
		fmt.Fprintln(os.Stderr, "colab-glm53 start: too many positional arguments")
		return 2
	}
	ts, ok := tiers[*tier]
	if !ok {
		fmt.Fprintf(os.Stderr, "colab-glm53 start: unsupported tier %q (use 2.51, 3.05, or 4.05)\n", *tier)
		return 2
	}
	if *ctxLen != 131072 && *ctxLen != 262144 {
		fmt.Fprintf(os.Stderr, "colab-glm53 start: unsupported --ctx %d (use 131072 or 262144)\n", *ctxLen)
		return 2
	}

	auth := checkAuthStatus(defaultProfile)
	if !auth.Authenticated {
		fmt.Fprintf(os.Stderr, "[colab-glm53] Not authenticated (%s).\nRun in a separate terminal window:\n  %s\n", auth.Reason, auth.LoginCommand)
		return 1
	}

	keyPath := resolveSSHKey()
	if err := ensureSSHKey(keyPath); err != nil {
		fmt.Fprintf(os.Stderr, "[colab-glm53] SSH key setup failed: %v\n", err)
		return 1
	}

	// Prevent macOS idle sleep during provisioning.
	if runtime.GOOS == "darwin" {
		if caf, err := exec.LookPath("caffeinate"); err == nil {
			c := exec.Command(caf, "-i", "-w", strconv.Itoa(os.Getpid()))
			_ = c.Start()
		}
	}

	fmt.Printf("\n=== GLM-5.3-Flash Uncensored · Colab G4 Launcher ===\n")
	fmt.Printf("  Session:    %s\n", *session)
	fmt.Printf("  GPU:        Colab G4 (RTX PRO 6000 Blackwell 96 GB, ~10.05 CU/hr)\n")
	fmt.Printf("  Quant:      %s bpw EXL3 (%s)\n", *tier, ts.HFRepo)
	fmt.Printf("  Context:    %d\n", *ctxLen)
	fmt.Printf("  Local Port: %d\n\n", *port)

	// Allocate session if not already active.
	stCmd := exec.Command(auth.ColabBin, "status", "-s", *session)
	stCmd.Stdin = bytes.NewReader(nil)
	stOut, stErr := stCmd.CombinedOutput()
	stLower := strings.ToLower(string(stOut))
	if stErr != nil || strings.Contains(stLower, "not found") || strings.Contains(stLower, "no active sessions") || strings.Contains(stLower, "does not exist") {
		if err := createG4Session(auth.ColabBin, *session, !*noWait); err != nil {
			fmt.Fprintf(os.Stderr, "[colab-glm53] %v\n", err)
			return 1
		}
	} else {
		fmt.Printf("[colab-glm53] Reusing active Colab session %q\n", *session)
	}

	// Stream remote provisioning script over SSH via colab ssh --proxy-mode.
	if err := runRemoteProvision(auth.ColabBin, keyPath, *session, ts, *ctxLen, *tier, !*noWait); err != nil {
		fmt.Fprintf(os.Stderr, "[colab-glm53] Remote provisioning failed: %v\n", err)
		fmt.Fprintf(os.Stderr, "If the Colab VM is still running, stop billing with:\n  colab-glm53 stop --session %s --port %d\n", *session, *port)
		return 1
	}

	modelName := fmt.Sprintf("glm-5.3-flash-uncensored-%sbpw", *tier)
	writeMetaFile(*session, *port, *tier, *ctxLen, modelName)

	tArgs := []string{"--session", *session, "--port", strconv.Itoa(*port)}
	if *bg {
		tArgs = append(tArgs, "--bg")
	}
	return cmdTunnel(tArgs)
}

func createG4Session(colabBin, session string, waitForGPU bool) error {
	baseDelays := []int{30, 45, 60, 90, 120}
	maxCap := envInt("COLAB_WAIT_CAP", 120)
	customBase := envInt("COLAB_RETRY_BASE", 0)
	maxAttempts := envInt("COLAB_MAX_ATTEMPTS", 0)

	fmt.Printf("[colab-glm53] Creating Colab G4 session %q (~10.05 CU/hr)...\n", session)
	attempt := 1
	for {
		// NOTE: Never pass --high-mem for G4; Colab rejects shape=hm for G4 and
		// already allocates the full 176 GiB RAM / 48 vCPU machine shape.
		cmd := exec.Command(colabBin, "new", "-s", session, "--gpu", "G4")
		cmd.Stdin = bytes.NewReader(nil)
		cmd.Env = append(os.Environ(), "COLAB_CLI_ASSIGN_RETRIES=0")
		outBytes, err := cmd.CombinedOutput()
		out := string(outBytes)

		if err == nil && !regexp.MustCompile(`(?i)Error:|Traceback|Service Unavailable|Allocation refused|Backend rejected`).MatchString(out) {
			fmt.Printf("[colab-glm53] Colab G4 session %q allocated (attempt #%d)\n", session, attempt)
			return nil
		}
		cleaned := strings.Join(strings.Fields(boxLineRe.ReplaceAllString(out, " ")), " ")
		if hardStopRe.MatchString(cleaned) {
			return fmt.Errorf("Colab rejected allocation (hard stop, not retrying):\n%s", strings.TrimSpace(out))
		}
		if waitForGPU && retryRe.MatchString(cleaned) {
			if maxAttempts > 0 && attempt >= maxAttempts {
				return fmt.Errorf("reached COLAB_MAX_ATTEMPTS=%d without available G4", maxAttempts)
			}
			delay := customBase
			if delay <= 0 {
				idx := attempt - 1
				if idx >= len(baseDelays) {
					idx = len(baseDelays) - 1
				}
				delay = baseDelays[idx]
			}
			if delay > maxCap {
				delay = maxCap
			}
			if delay < 1 {
				delay = 1
			}
			attempt++
			fmt.Printf("[colab-glm53] G4 busy (503). Waiting %ds before retry #%d...\n", delay, attempt)
			time.Sleep(time.Duration(delay) * time.Second)
			continue
		}
		return fmt.Errorf("failed to create Colab G4 session:\n%s", strings.TrimSpace(out))
	}
}

func runRemoteProvision(colabBin, keyPath, session string, ts tierSpec, ctxLen int, tier string, waitForGPU bool) error {
	const maxTries = 4
	proxyCmd := fmt.Sprintf("%s ssh --proxy-mode -s %s -i %s", shellQuote(colabBin), shellQuote(session), shellQuote(keyPath))
	remoteCmd := fmt.Sprintf("bash -s -- %s %s %d %s %s 0",
		shellQuote(ts.HFRepo), shellQuote(ts.ModelDir), ctxLen, shellQuote(ts.SpecMinP), shellQuote(tier))

	for try := 1; try <= maxTries; try++ {
		cmd := exec.Command("ssh",
			"-i", keyPath,
			"-o", "ProxyCommand="+proxyCmd,
			"-o", "StrictHostKeyChecking=no",
			"-o", "UserKnownHostsFile=/dev/null",
			"-o", "ServerAliveInterval=20",
			"-o", "ServerAliveCountMax=6",
			"-o", "ConnectTimeout=25",
			"root@colab",
			remoteCmd,
		)
		cmd.Stdin = strings.NewReader(remoteProvisionScript)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		err := cmd.Run()
		if err == nil {
			return nil
		}
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && (exitErr.ExitCode() == 42 || exitErr.ExitCode() == 130) {
			return fmt.Errorf("remote provisioning exited with code %d", exitErr.ExitCode())
		}
		if try < maxTries {
			st := exec.Command(colabBin, "status", "-s", session)
			st.Stdin = bytes.NewReader(nil)
			stOut, stErr := st.CombinedOutput()
			stLower := strings.ToLower(string(stOut))
			if stErr != nil || strings.Contains(stLower, "not found") || strings.Contains(stLower, "no active sessions") {
				if !waitForGPU {
					return fmt.Errorf("session %q ended during provisioning", session)
				}
				fmt.Printf("[colab-glm53] Session %q disappeared; re-allocating G4...\n", session)
				if err := createG4Session(colabBin, session, true); err != nil {
					return err
				}
				continue
			}
			waitSec := try * 5
			fmt.Fprintf(os.Stderr, "[colab-glm53] SSH/WebSocket interrupted (%v). Reconnecting (%d/%d) in %ds...\n", err, try, maxTries, waitSec)
			time.Sleep(time.Duration(waitSec) * time.Second)
		}
	}
	return fmt.Errorf("could not complete SSH provisioning after %d attempts", maxTries)
}

func cmdTunnel(args []string) int {
	fs := flag.NewFlagSet("tunnel", flag.ContinueOnError)
	session := fs.String("session", defaultSession, "Colab session name")
	port := fs.Int("port", defaultPort, "local port")
	bg := fs.Bool("bg", false, "run tunnel in background")
	daemon := fs.Bool("daemon", false, "internal background loop mode")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	colabBin, err := resolveColabBin()
	if err != nil {
		fmt.Fprintf(os.Stderr, "[tunnel] %v\n", err)
		return 1
	}
	keyPath := resolveSSHKey()
	if err := ensureSSHKey(keyPath); err != nil {
		fmt.Fprintf(os.Stderr, "[tunnel] %v\n", err)
		return 1
	}

	pidFile := fmt.Sprintf("/tmp/colab-tunnel-%s-%d.pid", *session, *port)
	logFile := fmt.Sprintf("/tmp/colab-tunnel-%s-%d.log", *session, *port)

	if *bg && !*daemon {
		if existing := readLivePID(pidFile); existing > 0 {
			if _, ok := probeModels(*port); ok {
				printReadySummary(*session, *port, fmt.Sprintf("background (already running, PID %d, log: %s)", existing, logFile))
				return 0
			}
			fmt.Printf("[tunnel] Already running (PID %d) on 127.0.0.1:%d (log: %s)\n", existing, *port, logFile)
			return 0
		}
		exe, err := os.Executable()
		if err != nil {
			fmt.Fprintf(os.Stderr, "[tunnel] cannot locate self executable: %v\n", err)
			return 1
		}
		lf, err := os.OpenFile(logFile, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
		if err != nil {
			fmt.Fprintf(os.Stderr, "[tunnel] cannot open log file %s: %v\n", logFile, err)
			return 1
		}
		child := exec.Command(exe, "tunnel", "--session", *session, "--port", strconv.Itoa(*port), "--daemon")
		child.Stdin = nil
		child.Stdout = lf
		child.Stderr = lf
		child.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		if err := child.Start(); err != nil {
			_ = lf.Close()
			fmt.Fprintf(os.Stderr, "[tunnel] failed to spawn background tunnel: %v\n", err)
			return 1
		}
		_ = lf.Close()
		_ = os.WriteFile(pidFile, []byte(strconv.Itoa(child.Process.Pid)+"\n"), 0o600)

		deadline := time.Now().Add(45 * time.Second)
		for time.Now().Before(deadline) {
			if !pidAlive(child.Process.Pid) {
				fmt.Fprintf(os.Stderr, "[tunnel] Background tunnel exited unexpectedly. Log (%s):\n", logFile)
				if b, err := os.ReadFile(logFile); err == nil {
					_, _ = os.Stderr.Write(b)
				}
				_ = os.Remove(pidFile)
				return 1
			}
			if _, ok := probeModels(*port); ok {
				printReadySummary(*session, *port, fmt.Sprintf("background (PID %d, log: %s)", child.Process.Pid, logFile))
				return 0
			}
			time.Sleep(1 * time.Second)
		}
		fmt.Printf("[tunnel] Started in background (PID %d), still waiting for /v1/models 200 OK. Log: %s\n", child.Process.Pid, logFile)
		return 0
	}

	// Foreground / daemon loop.
	sigCh := make(chan os.Signal, 2)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	proxyCmd := fmt.Sprintf("%s ssh --proxy-mode -s %s -i %s", shellQuote(colabBin), shellQuote(*session), shellQuote(keyPath))
	printed := false

	for {
		sshCmd := exec.Command("ssh",
			"-N",
			"-L", fmt.Sprintf("127.0.0.1:%d:127.0.0.1:%d", *port, defaultRemotePort),
			"-i", keyPath,
			"-o", "ProxyCommand="+proxyCmd,
			"-o", "StrictHostKeyChecking=no",
			"-o", "UserKnownHostsFile=/dev/null",
			"-o", "ExitOnForwardFailure=yes",
			"-o", "ServerAliveInterval=20",
			"-o", "ServerAliveCountMax=3",
			"-o", "ConnectTimeout=20",
			"root@colab",
		)
		sshCmd.Stdin = bytes.NewReader(nil)
		sshCmd.Stdout = os.Stdout
		sshCmd.Stderr = os.Stderr
		if err := sshCmd.Start(); err != nil {
			fmt.Fprintf(os.Stderr, "[tunnel] ssh start failed: %v\n", err)
			return 1
		}
		doneCh := make(chan error, 1)
		go func() { doneCh <- sshCmd.Wait() }()

		waitReady := time.NewTicker(1 * time.Second)
		readyLimit := time.After(60 * time.Second)
		exited := false
	pollLoop:
		for {
			select {
			case <-sigCh:
				waitReady.Stop()
				if sshCmd.Process != nil {
					_ = sshCmd.Process.Kill()
				}
				return 0
			case <-doneCh:
				waitReady.Stop()
				exited = true
				break pollLoop
			case <-readyLimit:
				waitReady.Stop()
				break pollLoop
			case <-waitReady.C:
				if _, ok := probeModels(*port); ok {
					waitReady.Stop()
					if !*daemon && !printed {
						printReadySummary(*session, *port, "foreground (Ctrl+C closes tunnel; Colab VM stays running)")
						printed = true
					}
					break pollLoop
				}
			}
		}

		if !exited {
			select {
			case <-sigCh:
				if sshCmd.Process != nil {
					_ = sshCmd.Process.Kill()
				}
				return 0
			case <-doneCh:
			}
		}
		fmt.Printf("[tunnel] Disconnected at %s. Reconnecting in 3s...\n", time.Now().Format("15:04:05"))
		select {
		case <-sigCh:
			return 0
		case <-time.After(3 * time.Second):
		}
	}
}

func cmdStop(args []string) int {
	fs := flag.NewFlagSet("stop", flag.ContinueOnError)
	session := fs.String("session", defaultSession, "Colab session name")
	port := fs.Int("port", defaultPort, "local port")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	pidFile := fmt.Sprintf("/tmp/colab-tunnel-%s-%d.pid", *session, *port)
	metaFile := fmt.Sprintf("/tmp/colab-tunnel-%s-%d.env", *session, *port)
	if pid := readLivePID(pidFile); pid > 0 {
		fmt.Printf("[stop] Stopping background tunnel PID %d...\n", pid)
		_ = syscall.Kill(pid, syscall.SIGTERM)
	}
	_ = os.Remove(pidFile)
	_ = os.Remove(metaFile)

	// Kill any lingering ssh port-forward for this local port.
	_ = exec.Command("pkill", "-f", fmt.Sprintf("127.0.0.1:%d:127.0.0.1:", *port)).Run()

	colabBin, err := resolveColabBin()
	if err != nil {
		fmt.Fprintf(os.Stderr, "[stop] Warning: %v; cleaned local tunnel only.\n", err)
		return 1
	}
	fmt.Printf("[stop] Stopping Colab session %q...\n", *session)
	stopCmd := exec.Command(colabBin, "stop", "-s", *session)
	stopCmd.Stdin = bytes.NewReader(nil)
	stopCmd.Stdout = os.Stdout
	stopCmd.Stderr = os.Stderr
	_ = stopCmd.Run()

	fmt.Println("\n=== Active sessions ===")
	sCmd := exec.Command(colabBin, "sessions")
	sCmd.Stdin = bytes.NewReader(nil)
	sCmd.Stdout = os.Stdout
	sCmd.Stderr = os.Stderr
	_ = sCmd.Run()

	fmt.Println("\n=== Compute unit usage (confirm Usage rate: 0.00/hr) ===")
	uCmd := exec.Command(colabBin, "usage")
	uCmd.Stdin = bytes.NewReader(nil)
	uCmd.Stdout = os.Stdout
	uCmd.Stderr = os.Stderr
	_ = uCmd.Run()
	return 0
}

func probeModels(port int) (string, bool) {
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/v1/models", port))
	if err != nil {
		return "", false
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", false
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	return string(body), true
}

func writeMetaFile(session string, port int, tier string, ctxLen int, modelName string) {
	meta := fmt.Sprintf("MODEL_TIER=%q\nMODEL_CTX=%d\nMODEL_NAME=%q\n", tier, ctxLen, modelName)
	_ = os.WriteFile(fmt.Sprintf("/tmp/colab-tunnel-%s-%d.env", session, port), []byte(meta), 0o600)
}

func printReadySummary(session string, port int, modeDesc string) {
	modelName := "glm-5.3-flash-uncensored-2.51bpw"
	if body, ok := probeModels(port); ok {
		var parsed struct {
			Data []struct {
				ID string `json:"id"`
			} `json:"data"`
		}
		if json.Unmarshal([]byte(body), &parsed) == nil && len(parsed.Data) > 0 && parsed.Data[0].ID != "" {
			modelName = parsed.Data[0].ID
		}
	}
	fmt.Printf(`
======================================================================
  GLM-5.3-Flash Uncensored — READY (200 OK)
======================================================================
  Status:             ONLINE (Colab session: %s, ~10.05 CU/hr)
  GPU:                NVIDIA RTX PRO 6000 Blackwell (96 GB)
  Tunnel Mode:        %s
  Model Name:         %s
  OpenAI Base URL:    http://127.0.0.1:%d/v1
  Anthropic Base URL: http://127.0.0.1:%d
  Web UI:             http://127.0.0.1:%d/
  API Key:            sk-local (any non-empty string)

  Claude Code snippet:
    ANTHROPIC_BASE_URL="http://127.0.0.1:%d" \
    ANTHROPIC_AUTH_TOKEN="sk-local" \
    ANTHROPIC_MODEL="%s" \
    claude

  STOP BILLING WHEN DONE:
    colab-glm53 stop --session %s --port %d
======================================================================
`, session, modeDesc, modelName, port, port, port, port, modelName, session, port)
}

func readLivePID(pidFile string) int {
	b, err := os.ReadFile(pidFile)
	if err != nil {
		return 0
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || pid <= 0 {
		return 0
	}
	if pidAlive(pid) {
		return pid
	}
	return 0
}

func pidAlive(pid int) bool {
	return syscall.Kill(pid, 0) == nil
}

func envInt(name string, def int) int {
	if v := strings.TrimSpace(os.Getenv(name)); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
