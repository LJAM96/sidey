// sidey-tvos-helper is Sidey's AGPL-3.0 Go helper for the tvOS provider
// (ADR-0003, LICENSING.md §2.1). It is a derivation of atvloadly (pinned
// commit df201956449635815d1d816d0eaf20c4baf4f9e6) that exposes the fork's
// core tvOS behaviour - Apple TV discovery, remote pairing and
// sign+install/refresh via the plumesign binary - over a line delimited
// JSON protocol on stdin/stdout. The Rust device agent supervises this
// binary as a child process (ADR-0003).
//
// Protocol: one JSON object per line on stdin, one JSON response object per
// line on stdout. Requests carry an "op" field and an "id" echoed on the
// response. Responses carry "ok" (bool) and either "result" or "error" +
// "error_stage" so the agent can attribute failures to discovery, pairing,
// signing or installation (Phase H exit criteria).
//
// Verbs:
//
//	ping            -> {"ok":true} (liveness + readiness probe)
//	deploy          -> sign and install/refresh an IPA on a device
//	verify          -> re-check post-install state against a signed recipe
//	inventory       -> installed apps recorded in the worker DB
//	uninstall       -> remove an installed app (best effort)
//	collect         -> diagnostic dump (logs + env facts) for proofs
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/bitxeno/atvloadly/internal/app"
)

const opVersion = "1"

// request is a decoded line from the agent. Fields are per-verb; unused
// fields are ignored by the handler for the verb.
type request struct {
	ID   string                     `json:"id"`
	Op   string                     `json:"op"`
	Data map[string]any             `json:"data"`
	Raw  json.RawMessage            `json:"-"`
	Meta map[string]json.RawMessage `json:"-"`
}

// response is the single JSON response line emitted per request.
type response struct {
	ID              string         `json:"id"`
	Op              string         `json:"op"`
	OK              bool           `json:"ok"`
	Result          map[string]any `json:"result,omitempty"`
	Error           string         `json:"error,omitempty"`
	ErrorStage      string         `json:"error_stage,omitempty"`
	PlumesignStderr string         `json:"plumesign_stderr,omitempty"`
}

var (
	dataDir    string
	debug      bool
	scriptsDir string
)

func main() {
	if err := bootstrap(); err != nil {
		dieErr(err, "bootstrap")
	}

	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	out := bufio.NewWriter(os.Stdout)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		req, err := parseRequest([]byte(line))
		if err != nil {
			write(out, response{Error: err.Error(), ErrorStage: "protocol"})
			continue
		}
		resp := dispatch(req)
		resp.ID = req.ID
		resp.Op = req.Op
		write(out, resp)
	}
	if err := sc.Err(); err != nil {
		die(err.Error(), "stdin")
	}
	_ = out.Flush()
}

func die(msg, stage string) {
	out, _ := json.Marshal(response{OK: false, Error: msg, ErrorStage: stage})
	fmt.Fprintln(os.Stderr, string(out))
	os.Exit(1)
}

func dieErr(err error, stage string) {
	die(err.Error(), stage)
}

// bootstrap wires the atvloadly configuration, settings, logging and DB so
// the forked managers function identically to the upstream server.
func bootstrap() error {
	dataDir = os.Getenv("SIDEY_TVOS_DATA_DIR")
	if dataDir == "" {
		dataDir = "/var/lib/sidey/tvos"
	}
	_ = os.MkdirAll(dataDir, 0o755)

	conf, err := app.InitConfig("", false)
	if err != nil {
		return fmt.Errorf("init config: %w", err)
	}
	conf.Server.DataDir = dataDir
	conf.Db.Path = dataDir
	if _, err := app.InitSettings(conf, false); err != nil {
		return fmt.Errorf("init settings: %w", err)
	}
	if err := app.InitLogger(conf); err != nil {
		return fmt.Errorf("init logger: %w", err)
	}
	if err := app.InitDb(conf); err != nil {
		return fmt.Errorf("init db: %w", err)
	}

	// The fork's managers default to a plumesign binary on PATH; allow an
	// explicit override for the agent-provided payload.
	if dir := os.Getenv("SIDEY_PLUMESIGN_DIR"); dir != "" {
		os.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	}

	// Optional repo scripts dir; when set, the deploy verb delegates the
	// proven pmd3 tunnel + InstallationProxy flow to tvos-install.sh (the
	// plumesign sign-rsd path fails pair-verify for pmd3 pair records).
	scriptsDir = os.Getenv("SIDEY_TVOS_SCRIPTS_DIR")
	return nil
}
