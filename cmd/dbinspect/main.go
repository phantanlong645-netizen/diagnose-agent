// Temporary database inspection tool. Not part of the shipped application.
// Usage: go run ./cmd/dbinspect <diagnostics.db>
package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: dbinspect <diagnostics.db>")
		os.Exit(2)
	}
	src := os.Args[1]
	tmpDir, err := os.MkdirTemp("", "dbinspect-")
	if err != nil {
		die("temp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)
	// Copy db + wal + shm so we never touch the live database.
	for _, suffix := range []string{"", "-wal", "-shm"} {
		s := src + suffix
		data, err := os.ReadFile(s)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			die("read %s: %w", s, err)
		}
		dst := filepath.Join(tmpDir, filepath.Base(src)+suffix)
		if err := os.WriteFile(dst, data, 0600); err != nil {
			die("write %s: %w", dst, err)
		}
	}
	copyPath := filepath.Join(tmpDir, filepath.Base(src))
	dsn := "file:" + copyPath + "?mode=ro&_busy_timeout=2000"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		die("open: %w", err)
	}
	defer db.Close()

	// Force a WAL checkpoint on the copy so all data is visible in read-only mode.
	_, _ = db.Exec("PRAGMA wal_checkpoint(TRUNCATE)")

	// Remaining positional args (drop the db path and any --flag tokens).
	var pos []string
	for _, a := range os.Args[2:] {
		if strings.HasPrefix(a, "--") {
			continue
		}
		pos = append(pos, a)
	}
	target := ""
	if len(pos) > 0 {
		target = pos[0]
	}
	switch {
	case flag("conversations"):
		listConversations(db)
	case flag("runs"):
		listRuns(db, target)
	case flag("events"):
		showEvents(db, target)
	case flag("evidence"):
		findEvidence(db, target)
	case flag("context"):
		showContext(db, target)
	case flag("dump"):
		dumpMessages(db, target)
	case flag("fingerprints"):
		showFingerprints(db, target)
	case flag("full"):
		fullReport(db)
	default:
		fullReport(db)
	}
}

func flag(name string) bool {
	for _, a := range os.Args[1:] {
		if a == "--"+name {
			return true
		}
	}
	return false
}

func die(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "dbinspect: "+format+"\n", args...)
	os.Exit(1)
}

type conv struct {
	ID        string
	ProfileID string
	Title     string
	Status    string
	CreatedAt string
	UpdatedAt string
}

func listConversations(db *sql.DB) {
	rows, err := db.Query(`SELECT id, profile_id, title, status, created_at, updated_at
		FROM conversations ORDER BY updated_at DESC`)
	ce(err)
	defer rows.Close()
	fmt.Println("ID | PROFILE | STATUS | UPDATED | TITLE")
	for rows.Next() {
		var c conv
		ce(rows.Scan(&c.ID, &c.ProfileID, &c.Title, &c.Status, &c.CreatedAt, &c.UpdatedAt))
		title := c.Title
		if len(title) > 60 {
			title = title[:60] + "..."
		}
		fmt.Printf("%s | %s | %s | %s | %s\n", c.ID, c.ProfileID, c.Status, c.UpdatedAt, title)
	}
	ce(rows.Err())
}

type run struct {
	ID           string
	Conversation string
	Profile      string
	Goal         string
	Status       string
	StartedAt    string
	EndedAt      sql.NullString
	Error        string
}

func listRuns(db *sql.DB, convID string) {
	rows, err := db.Query(`SELECT id, conversation_id, profile_id, goal, status, started_at, COALESCE(ended_at,''), COALESCE(error,'')
		FROM runs WHERE conversation_id = ? ORDER BY started_at ASC`, convID)
	ce(err)
	defer rows.Close()
	for rows.Next() {
		var r run
		ce(rows.Scan(&r.ID, &r.Conversation, &r.Profile, &r.Goal, &r.Status, &r.StartedAt, &r.EndedAt, &r.Error))
		fmt.Printf("\n=== RUN %s ===\n", r.ID)
		fmt.Printf("  status     : %s\n", r.Status)
		fmt.Printf("  started    : %s\n", r.StartedAt)
		if r.EndedAt.Valid && r.EndedAt.String != "" {
			fmt.Printf("  ended      : %s\n", r.EndedAt.String)
		}
		fmt.Printf("  goal       : %s\n", clip(r.Goal, 120))
		if r.Error != "" {
			fmt.Printf("  error      : %s\n", clip(r.Error, 200))
		}
	}
	ce(rows.Err())
}

func showEvents(db *sql.DB, runID string) {
	// If runID looks like a conversation (has runs), show across the conversation.
	scope := "run_id = ?"
	if _, err := db.Exec("SELECT 1 FROM runs WHERE id = ? LIMIT 1", runID); err == nil {
		// it's a run
	} else {
		scope = "conversation_id = ?"
	}
	q := fmt.Sprintf(`SELECT sequence, id, run_id, type, timestamp, payload_json
		FROM events WHERE %s ORDER BY sequence ASC`, scope)
	rows, err := db.Query(q, runID)
	ce(err)
	defer rows.Close()
	for rows.Next() {
		var seq int
		var id, rid, typ, ts string
		var payload []byte
		ce(rows.Scan(&seq, &id, &rid, &typ, &ts, &payload))
		fmt.Printf("\n--- #%d %s [%s] run=%s ---\n", seq, ts, typ, rid)
		printPayload(typ, payload)
	}
	ce(rows.Err())
}

func printPayload(typ string, payload []byte) {
	// Pretty-print depending on type; always fall back to compact JSON.
	switch typ {
	case "agent.message":
		var m struct {
			Message string `json:"message"`
		}
		if json.Unmarshal(payload, &m) == nil {
			fmt.Printf("  message: %s\n", clip(m.Message, 600))
			return
		}
	case "model.http.trace":
		var p struct {
			TraceID              string `json:"traceId"`
			LogicalCallID        string `json:"logicalCallId"`
			Stage                string `json:"stage"`
			WorkerStepID         string `json:"workerStepId"`
			WorkerRole           string `json:"workerRole"`
			Sequence             int64  `json:"sequence"`
			Attempt              int    `json:"attempt"`
			Model                string `json:"model"`
			MessageCount         int    `json:"messageCount"`
			MessageBytes         int    `json:"messageBytes"`
			ToolCount            int    `json:"toolCount"`
			Method               string `json:"method"`
			Scheme               string `json:"scheme"`
			Host                 string `json:"host"`
			Path                 string `json:"path"`
			RoundTrips           int    `json:"roundTrips"`
			GotConnection        bool   `json:"gotConnection"`
			ConnectionReused     bool   `json:"connectionReused"`
			WroteRequest         bool   `json:"wroteRequest"`
			FailurePhase         string `json:"failurePhase"`
			ErrorKind            string `json:"errorKind"`
			Error                string `json:"error"`
			StatusCode           int    `json:"statusCode"`
			TotalMS              int64  `json:"totalMs"`
			FirstByteMS          int64  `json:"firstByteMs"`
			ResponseBytes        int64  `json:"responseBytes"`
			BodyReadFailed       bool   `json:"bodyReadFailed"`
			BodyReachedEOF       bool   `json:"bodyReachedEOF"`
			BodyReadErrorKind    string `json:"bodyReadErrorKind"`
			HeadersReceived      bool   `json:"headersReceived"`
			GotFirstResponseByte bool   `json:"gotFirstResponseByte"`
			RequestID            string `json:"requestId"`
			CloudflareRay        string `json:"cloudflareRay"`
		}
		if json.Unmarshal(payload, &p) == nil {
			fmt.Printf("  trace: %s  call: %s  stage: %s  sequence: %d", p.TraceID, p.LogicalCallID, p.Stage, p.Sequence)
			if p.Attempt > 0 {
				fmt.Printf("  attempt: %d", p.Attempt)
			}
			fmt.Printf("  model: %s\n", p.Model)
			if p.WorkerStepID != "" {
				fmt.Printf("  worker: %s (%s)\n", p.WorkerStepID, p.WorkerRole)
			}
			fmt.Printf("  HTTP: %s %s://%s%s  roundTrips=%d connected=%v reused=%v wrote=%v\n", p.Method, p.Scheme, p.Host, p.Path, p.RoundTrips, p.GotConnection, p.ConnectionReused, p.WroteRequest)
			fmt.Printf("  request: messages=%d bytes=%d tools=%d  response: status=%d headers=%v firstByte=%v bytes=%d\n", p.MessageCount, p.MessageBytes, p.ToolCount, p.StatusCode, p.HeadersReceived, p.GotFirstResponseByte, p.ResponseBytes)
			if p.BodyReadFailed || p.BodyReadErrorKind != "" {
				fmt.Printf("  body: failed=%v reachedEOF=%v errorKind=%s\n", p.BodyReadFailed, p.BodyReachedEOF, p.BodyReadErrorKind)
			}
			fmt.Printf("  timing: total=%dms firstByte=%dms  outcome: %s/%s\n", p.TotalMS, p.FirstByteMS, p.FailurePhase, p.ErrorKind)
			if p.RequestID != "" || p.CloudflareRay != "" {
				fmt.Printf("  requestId: %s  cfRay: %s\n", p.RequestID, p.CloudflareRay)
			}
			if p.Error != "" {
				fmt.Printf("  error: %s\n", p.Error)
			}
			return
		}
	case "tool.proposed":
		var p struct {
			CallID    string         `json:"callId"`
			Name      string         `json:"name"`
			Tool      string         `json:"tool"`
			Arguments map[string]any `json:"arguments"`
		}
		if json.Unmarshal(payload, &p) == nil {
			if p.Tool == "" {
				p.Tool = p.Name
			}
			args, _ := json.Marshal(p.Arguments)
			fmt.Printf("  tool: %s  callId: %s\n  args: %s\n", p.Tool, p.CallID, clip(string(args), 400))
			return
		}
	case "tool.started":
		var p struct {
			CallID  string `json:"callId"`
			Name    string `json:"name"`
			Tool    string `json:"tool"`
			Summary string `json:"summary"`
		}
		if json.Unmarshal(payload, &p) == nil {
			if p.Tool == "" {
				p.Tool = p.Name
			}
			fmt.Printf("  tool: %s  callId: %s  summary: %s\n", p.Tool, p.CallID, clip(p.Summary, 200))
			return
		}
	case "tool.completed", "tool.failed":
		var p struct {
			CallID  string `json:"callId"`
			Name    string `json:"name"`
			Tool    string `json:"tool"`
			Success *bool  `json:"successful"`
			Summary string `json:"summary"`
			Message string `json:"message"`
			Error   string `json:"error"`
		}
		if json.Unmarshal(payload, &p) == nil {
			if p.Tool == "" {
				p.Tool = p.Name
			}
			success := typ == "tool.completed"
			if p.Success != nil {
				success = *p.Success
			}
			fmt.Printf("  tool: %s  success: %v  callId: %s\n", p.Tool, success, p.CallID)
			if p.Summary != "" {
				fmt.Printf("  summary: %s\n", clip(p.Summary, 300))
			}
			if p.Message != "" {
				fmt.Printf("  message: %s\n", clip(p.Message, 600))
			}
			if p.Error != "" {
				fmt.Printf("  error: %s\n", clip(p.Error, 600))
			}
			return
		}
	case "evidence.captured":
		var e struct {
			ID   string          `json:"id"`
			Kind string          `json:"kind"`
			Size int             `json:"size"`
			Data json.RawMessage `json:"data"`
		}
		if json.Unmarshal(payload, &e) == nil {
			if e.Size == 0 {
				e.Size = len(e.Data)
			}
			fmt.Printf("  evidenceId: %s  kind: %s  size: %d\n", e.ID, e.Kind, e.Size)
			return
		}
	}
	// Fallback: print compact JSON, clipped.
	var raw any
	if json.Unmarshal(payload, &raw) == nil {
		pretty, _ := json.MarshalIndent(raw, "  ", "  ")
		fmt.Printf("  payload: %s\n", clip(string(pretty), 800))
	} else {
		fmt.Printf("  payload(raw): %s\n", clip(string(payload), 400))
	}
}

func findEvidence(db *sql.DB, evidenceID string) {
	// Find which run captured this evidence across all conversations.
	q := `SELECT e.run_id, e.conversation_id, e.timestamp, e.payload_json
		FROM events e
		WHERE e.type = 'evidence.captured'
		ORDER BY e.sequence ASC`
	rows, err := db.Query(q)
	ce(err)
	defer rows.Close()
	found := false
	for rows.Next() {
		var rid, cid, ts string
		var payload []byte
		ce(rows.Scan(&rid, &cid, &ts, &payload))
		var e struct {
			ID   string          `json:"id"`
			Kind string          `json:"kind"`
			Size int             `json:"size"`
			Data json.RawMessage `json:"data"`
		}
		if json.Unmarshal(payload, &e) != nil {
			continue
		}
		if strings.Contains(e.ID, evidenceID) {
			if e.Size == 0 {
				e.Size = len(e.Data)
			}
			found = true
			fmt.Printf("MATCH: evidenceId=%s  run=%s  conversation=%s  captured=%s  kind=%s  size=%d\n",
				e.ID, rid, cid, ts, e.Kind, e.Size)
		}
	}
	ce(rows.Err())
	if !found {
		fmt.Printf("No evidence.captured event found matching ID containing %q\n", evidenceID)
	}
	// Also find read_evidence tool calls referencing this ID, to see which run tried to read it.
	fmt.Println("\n-- read_evidence calls referencing this ID --")
	q2 := `SELECT sequence, run_id, timestamp, payload_json FROM events
		WHERE type IN ('tool.proposed','tool.completed','tool.failed')
		ORDER BY sequence ASC`
	rows2, err := db.Query(q2)
	ce(err)
	defer rows2.Close()
	for rows2.Next() {
		var seq int
		var rid, ts string
		var payload []byte
		ce(rows2.Scan(&seq, &rid, &ts, &payload))
		if !strings.Contains(string(payload), evidenceID) {
			continue
		}
		var p struct {
			Tool      string         `json:"tool"`
			Arguments map[string]any `json:"arguments"`
			Success   bool           `json:"successful"`
			Error     string         `json:"error"`
			Message   string         `json:"message"`
		}
		_ = json.Unmarshal(payload, &p)
		if p.Tool != "read_evidence" {
			continue
		}
		args, _ := json.Marshal(p.Arguments)
		fmt.Printf("#%d [%s] run=%s success=%v\n  args: %s\n", seq, ts, rid, p.Success, clip(string(args), 300))
		if p.Error != "" {
			fmt.Printf("  error: %s\n", clip(p.Error, 300))
		}
		if p.Message != "" {
			fmt.Printf("  message: %s\n", clip(p.Message, 300))
		}
	}
	ce(rows2.Err())
}

func showContext(db *sql.DB, convID string) {
	var rev int
	var srcRun string
	var updated string
	var msgs []byte
	err := db.QueryRow(`SELECT revision, source_run_id, updated_at, messages_json
		FROM conversation_contexts WHERE conversation_id = ?`, convID).
		Scan(&rev, &srcRun, &updated, &msgs)
	if err == sql.ErrNoRows {
		fmt.Println("no conversation_contexts row for", convID)
		return
	}
	ce(err)
	fmt.Printf("revision=%d  source_run=%s  updated=%s\n", rev, srcRun, updated)
	var arr []map[string]any
	if json.Unmarshal(msgs, &arr) != nil {
		fmt.Println("messages_json is not an array; raw length:", len(msgs))
		return
	}
	fmt.Printf("message count: %d\n", len(arr))
	totalBytes := 0
	full := flag("full")
	for i, m := range arr {
		role, _ := m["role"].(string)
		content, _ := m["content"].(string)
		totalBytes += len(content)
		toolCalls := ""
		if tc, ok := m["tool_calls"].([]any); ok && len(tc) > 0 {
			toolCalls = fmt.Sprintf("  tool_calls=%d", len(tc))
		}
		preview := clip(content, 220)
		if full {
			preview = content
		}
		fmt.Printf("\n===== [%d] %-9s%s (%d bytes) =====\n%s\n", i, role, toolCalls, len(content), preview)
	}
	fmt.Printf("\ntotal content bytes: %d\n", totalBytes)
}

func dumpMessages(db *sql.DB, convID string) {
	var msgs []byte
	err := db.QueryRow(`SELECT messages_json FROM conversation_contexts WHERE conversation_id = ?`, convID).Scan(&msgs)
	if err == sql.ErrNoRows {
		fmt.Println("no context row")
		return
	}
	ce(err)
	var arr []map[string]any
	ce(json.Unmarshal(msgs, &arr))
	outPath := filepath.Join(os.TempDir(), "ctx_messages.json")
	pretty, _ := json.MarshalIndent(arr, "", "  ")
	ce(os.WriteFile(outPath, pretty, 0600))
	fmt.Printf("wrote %d messages (%d bytes) to %s\n", len(arr), len(pretty), outPath)
	// Also print short index of role + content length + evidence-id hits.
	for i, m := range arr {
		role, _ := m["role"].(string)
		content, _ := m["content"].(string)
		hits := ""
		for _, id := range []string{"c0f277cf", "39376491", "c265a106"} {
			if strings.Contains(content, id) {
				hits += " " + id
			}
		}
		fmt.Printf("  [%d] %-9s len=%d%s\n", i, role, len(content), hits)
	}
}

func showFingerprints(db *sql.DB, convID string) {
	rows, err := db.Query(`SELECT fingerprint, tool_name, evidence_id, successful, last_seen
		FROM tool_fingerprints WHERE conversation_id = ? ORDER BY last_seen DESC`, convID)
	ce(err)
	defer rows.Close()
	for rows.Next() {
		var fp, tool, eid string
		var succ int
		var ls string
		ce(rows.Scan(&fp, &tool, &eid, &succ, &ls))
		fmt.Printf("  fp=%s tool=%s evidence=%s success=%d last_seen=%s\n", clip(fp, 24), tool, eid, succ, ls)
	}
	ce(rows.Err())
}

func fullReport(db *sql.DB) {
	// Find the most recently updated active conversation.
	var c conv
	err := db.QueryRow(`SELECT id, profile_id, title, status, created_at, updated_at
		FROM conversations ORDER BY updated_at DESC LIMIT 1`).
		Scan(&c.ID, &c.ProfileID, &c.Title, &c.Status, &c.CreatedAt, &c.UpdatedAt)
	ce(err)
	fmt.Printf("########## MOST RECENT CONVERSATION ##########\n")
	fmt.Printf("id        : %s\n", c.ID)
	fmt.Printf("profile   : %s\n", c.ProfileID)
	fmt.Printf("status    : %s\n", c.Status)
	fmt.Printf("created   : %s\n", c.CreatedAt)
	fmt.Printf("updated   : %s\n", c.UpdatedAt)
	fmt.Printf("title     : %s\n", c.Title)

	// List runs.
	fmt.Printf("\n########## RUNS (oldest -> newest) ##########\n")
	listRuns(db, c.ID)

	// Evidence captured across the whole conversation, grouped by run.
	fmt.Printf("\n########## EVIDENCE CAPTURED (by run) ##########\n")
	rows, err := db.Query(`SELECT run_id, payload_json FROM events
		WHERE conversation_id = ? AND type = 'evidence.captured'
		ORDER BY sequence ASC`, c.ID)
	ce(err)
	byRun := map[string][]string{}
	for rows.Next() {
		var rid string
		var payload []byte
		ce(rows.Scan(&rid, &payload))
		var e struct {
			ID   string `json:"id"`
			Kind string `json:"kind"`
			Size int    `json:"size"`
		}
		if json.Unmarshal(payload, &e) != nil {
			continue
		}
		byRun[rid] = append(byRun[rid], fmt.Sprintf("%s (kind=%s size=%d)", e.ID, e.Kind, e.Size))
	}
	ce(rows.Err())
	rows.Close()
	runIDs := make([]string, 0, len(byRun))
	for rid := range byRun {
		runIDs = append(runIDs, rid)
	}
	sort.Slice(runIDs, func(i, j int) bool { return len(byRun[runIDs[i]]) < len(byRun[runIDs[j]]) })
	for _, rid := range runIDs {
		fmt.Printf("\n  run %s:\n", rid)
		for _, line := range byRun[rid] {
			fmt.Printf("    - %s\n", line)
		}
	}

	// Look specifically for the suspect evidence IDs.
	fmt.Printf("\n########## SUSPECT EVIDENCE IDS ##########\n")
	for _, id := range []string{"c0f277cf", "39376491", "c265a106"} {
		fmt.Printf("\n-- searching %s --\n", id)
		findEvidence(db, id)
	}

	// Last N events of the most recent run.
	fmt.Printf("\n########## CONTEXT SNAPSHOT ##########\n")
	showContext(db, c.ID)

	fmt.Printf("\n########## TOOL FINGERPRINTS ##########\n")
	showFingerprints(db, c.ID)
}

func clip(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\r", "")
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func ce(err error) {
	if err != nil {
		die("query: %w", err)
	}
}

// keep time import used (timestamps parsed elsewhere if needed).
var _ = time.Now
