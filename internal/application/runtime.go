package application

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	_ "modernc.org/sqlite"

	"olt-diagnostic-agent/internal/domain"
)

// Journal 是诊断数据的 SQLite 持久化实现，满足 Runner 的 RunJournal 接口。
type Journal struct {
	db *sql.DB
}

// initialConversationSchema 是迁移 1 建表语句：会话、运行、事件与会话上下文
// 四张核心表。one_active_conversation_per_profile 唯一索引保证每个目标档案
// 同时只有一个 active 会话。
const initialConversationSchema = `
CREATE TABLE conversations (
    id TEXT PRIMARY KEY,
    profile_id TEXT NOT NULL,
    title TEXT NOT NULL DEFAULT '',
    status TEXT NOT NULL CHECK (status IN ('active', 'archived')),
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);
CREATE UNIQUE INDEX one_active_conversation_per_profile
    ON conversations(profile_id) WHERE status = 'active';

CREATE TABLE runs (
    id TEXT PRIMARY KEY,
    conversation_id TEXT NOT NULL REFERENCES conversations(id) ON DELETE CASCADE,
    profile_id TEXT NOT NULL,
    goal TEXT NOT NULL,
    status TEXT NOT NULL,
    started_at TEXT NOT NULL,
    ended_at TEXT,
    error TEXT NOT NULL DEFAULT ''
);
CREATE INDEX runs_by_conversation ON runs(conversation_id, started_at);

CREATE TABLE events (
    sequence INTEGER PRIMARY KEY AUTOINCREMENT,
    id TEXT NOT NULL UNIQUE,
    conversation_id TEXT NOT NULL REFERENCES conversations(id) ON DELETE CASCADE,
    run_id TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    type TEXT NOT NULL,
    timestamp TEXT NOT NULL,
    payload_json BLOB NOT NULL
);
CREATE INDEX events_by_conversation ON events(conversation_id, sequence);

CREATE TABLE conversation_contexts (
    conversation_id TEXT PRIMARY KEY REFERENCES conversations(id) ON DELETE CASCADE,
    format_version INTEGER NOT NULL,
    revision INTEGER NOT NULL,
    source_run_id TEXT NOT NULL REFERENCES runs(id),
    messages_json BLOB NOT NULL,
    updated_at TEXT NOT NULL
);`

// contextMemorySchema 是迁移 2 建表语句：上下文事实（可按键替换的持久记忆）、
// 诊断计划与工具指纹，用于跨 run 的记忆保留与只读结果复用。
const contextMemorySchema = `
CREATE TABLE context_facts (
    conversation_id TEXT NOT NULL REFERENCES conversations(id) ON DELETE CASCADE,
    fact_key TEXT NOT NULL,
    kind TEXT NOT NULL,
    status TEXT NOT NULL,
    content_json BLOB NOT NULL,
    evidence_ids_json BLOB NOT NULL DEFAULT '[]',
    source_tool TEXT NOT NULL DEFAULT '',
    source_locator TEXT NOT NULL DEFAULT '',
    confidence REAL NOT NULL DEFAULT 0,
    first_seen TEXT NOT NULL,
    last_seen TEXT NOT NULL,
    revision INTEGER NOT NULL,
    PRIMARY KEY (conversation_id, fact_key)
);
CREATE INDEX context_facts_by_conversation
    ON context_facts(conversation_id, last_seen DESC);

CREATE TABLE context_plans (
    conversation_id TEXT PRIMARY KEY REFERENCES conversations(id) ON DELETE CASCADE,
    plan_json BLOB NOT NULL,
    revision INTEGER NOT NULL,
    updated_at TEXT NOT NULL
);

CREATE TABLE tool_fingerprints (
    conversation_id TEXT NOT NULL REFERENCES conversations(id) ON DELETE CASCADE,
    fingerprint TEXT NOT NULL,
    tool_name TEXT NOT NULL,
    evidence_id TEXT NOT NULL DEFAULT '',
    successful INTEGER NOT NULL,
    last_seen TEXT NOT NULL,
    PRIMARY KEY (conversation_id, fingerprint)
);
CREATE INDEX tool_fingerprints_by_conversation
    ON tool_fingerprints(conversation_id, last_seen DESC);`

// tokenUsageSchema 是迁移 3 建表语句：按 run/会话记录每次模型生成的
// token 消耗与耗时。
const tokenUsageSchema = `
CREATE TABLE token_usage (
    id TEXT PRIMARY KEY,
    run_id TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    conversation_id TEXT NOT NULL REFERENCES conversations(id) ON DELETE CASCADE,
    iteration INTEGER NOT NULL,
    input_tokens INTEGER NOT NULL,
    output_tokens INTEGER NOT NULL,
    total_tokens INTEGER NOT NULL,
    elapsed_ms INTEGER NOT NULL,
    recorded_at TEXT NOT NULL
);
CREATE INDEX token_usage_by_conversation
    ON token_usage(conversation_id, iteration);`

// runModeSchema 是迁移 4 语句：为 runs 表补充诊断模式列（agent/team/manual）。
const runModeSchema = `ALTER TABLE runs ADD COLUMN mode TEXT NOT NULL DEFAULT 'agent';`

// NewJournal 打开（必要时创建）诊断数据库，配置 WAL 与忙等待超时，
// 执行 schema 迁移，并恢复上次中断的 run。
func NewJournal(path string) (*Journal, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, fmt.Errorf("create diagnostics database directory: %w", err)
	}
	databaseURL := &url.URL{Scheme: "file", Path: "/" + filepath.ToSlash(path)}
	query := databaseURL.Query()
	query.Set("_busy_timeout", "5000")
	query.Set("_foreign_keys", "on")
	query.Set("_journal_mode", "wal")
	databaseURL.RawQuery = query.Encode()
	db, err := sql.Open("sqlite", databaseURL.String())
	if err != nil {
		return nil, fmt.Errorf("open diagnostics database: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	journal := &Journal{db: db}
	for _, pragma := range []string{
		"PRAGMA foreign_keys = ON",
		"PRAGMA journal_mode = WAL",
		"PRAGMA busy_timeout = 5000",
	} {
		if _, err = db.Exec(pragma); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("configure diagnostics database: %w", err)
		}
	}
	if err = journal.migrate(); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err = journal.recoverInterruptedRuns(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return journal, nil
}

// Close 关闭底层数据库连接。
func (j *Journal) Close() error {
	return j.db.Close()
}

// migrate 从 schema_migrations 记录的当前版本逐步升级到版本 4：
// 每个版本在一个事务内应用对应 schema 并登记版本号。
func (j *Journal) migrate() error {
	if _, err := j.db.Exec(`CREATE TABLE IF NOT EXISTS schema_migrations (
        version INTEGER PRIMARY KEY,
        applied_at TEXT NOT NULL
    )`); err != nil {
		return fmt.Errorf("create schema migration table: %w", err)
	}
	var version int
	if err := j.db.QueryRow("SELECT COALESCE(MAX(version), 0) FROM schema_migrations").Scan(&version); err != nil {
		return fmt.Errorf("read diagnostics database version: %w", err)
	}
	if version > 4 {
		return fmt.Errorf("diagnostics database schema version %d is newer than supported version 4", version)
	}
	if version == 0 {
		tx, err := j.db.Begin()
		if err != nil {
			return fmt.Errorf("begin diagnostics database migration 1: %w", err)
		}
		if _, err = tx.Exec(initialConversationSchema); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("apply diagnostics database migration 1: %w", err)
		}
		if _, err = tx.Exec("INSERT INTO schema_migrations(version, applied_at) VALUES(1, ?)", databaseTime(time.Now().UTC())); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("record diagnostics database migration 1: %w", err)
		}
		if err = tx.Commit(); err != nil {
			return fmt.Errorf("commit diagnostics database migration 1: %w", err)
		}
		version = 1
	}
	if version == 1 {
		tx, err := j.db.Begin()
		if err != nil {
			return fmt.Errorf("begin diagnostics database migration 2: %w", err)
		}
		if _, err = tx.Exec(contextMemorySchema); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("apply diagnostics database migration 2: %w", err)
		}
		if _, err = tx.Exec("INSERT INTO schema_migrations(version, applied_at) VALUES(2, ?)", databaseTime(time.Now().UTC())); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("record diagnostics database migration 2: %w", err)
		}
		if err = tx.Commit(); err != nil {
			return fmt.Errorf("commit diagnostics database migration 2: %w", err)
		}
		version = 2
	}
	if version == 2 {
		tx, err := j.db.Begin()
		if err != nil {
			return fmt.Errorf("begin diagnostics database migration 3: %w", err)
		}
		if _, err = tx.Exec(tokenUsageSchema); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("apply diagnostics database migration 3: %w", err)
		}
		if _, err = tx.Exec("INSERT INTO schema_migrations(version, applied_at) VALUES(3, ?)", databaseTime(time.Now().UTC())); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("record diagnostics database migration 3: %w", err)
		}
		if err = tx.Commit(); err != nil {
			return fmt.Errorf("commit diagnostics database migration 3: %w", err)
		}
		version = 3
	}
	if version == 3 {
		tx, err := j.db.Begin()
		if err != nil {
			return fmt.Errorf("begin diagnostics database migration 4: %w", err)
		}
		if _, err = tx.Exec(runModeSchema); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("apply diagnostics database migration 4: %w", err)
		}
		if _, err = tx.Exec("INSERT INTO schema_migrations(version, applied_at) VALUES(4, ?)", databaseTime(time.Now().UTC())); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("record diagnostics database migration 4: %w", err)
		}
		if err = tx.Commit(); err != nil {
			return fmt.Errorf("commit diagnostics database migration 4: %w", err)
		}
	}
	return nil
}

// recoverInterruptedRuns 将上次进程退出时仍处于 running 的 run 标记为失败，
// 并为缺少上下文快照的会话注入一条"已恢复"的用户消息，避免对话丢失。
func (j *Journal) recoverInterruptedRuns() error {
	rows, err := j.db.Query(`SELECT id, conversation_id, profile_id, goal, mode, started_at
        FROM runs WHERE status = 'running' ORDER BY started_at`)
	if err != nil {
		return fmt.Errorf("read interrupted diagnostic runs: %w", err)
	}
	interrupted := make([]domain.Run, 0)
	for rows.Next() {
		var run domain.Run
		var startedAt string
		if err = rows.Scan(&run.ID, &run.ConversationID, &run.ProfileID, &run.Goal, &run.Mode, &startedAt); err != nil {
			_ = rows.Close()
			return fmt.Errorf("scan interrupted diagnostic run: %w", err)
		}
		if run.StartedAt, err = parseDatabaseTime(startedAt); err != nil {
			_ = rows.Close()
			return err
		}
		interrupted = append(interrupted, run)
	}
	if err = rows.Close(); err != nil {
		return fmt.Errorf("close interrupted diagnostic runs: %w", err)
	}
	if err = rows.Err(); err != nil {
		return fmt.Errorf("iterate interrupted diagnostic runs: %w", err)
	}
	if len(interrupted) == 0 {
		return nil
	}

	tx, err := j.db.Begin()
	if err != nil {
		return fmt.Errorf("begin interrupted-run recovery: %w", err)
	}
	defer tx.Rollback()
	for _, run := range interrupted {
		endedAt := time.Now().UTC()
		run.Status = domain.RunFailed
		run.EndedAt = &endedAt
		runError := "application stopped before the diagnostic run completed"
		if _, err = tx.Exec("UPDATE runs SET status = ?, ended_at = ?, error = ? WHERE id = ?", run.Status, databaseTime(endedAt), runError, run.ID); err != nil {
			return fmt.Errorf("recover interrupted diagnostic run %s: %w", run.ID, err)
		}
		var contextExists int
		if err = tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM conversation_contexts WHERE conversation_id = ?)`, run.ConversationID).Scan(&contextExists); err != nil {
			return fmt.Errorf("check interrupted conversation context %s: %w", run.ConversationID, err)
		}
		if contextExists == 0 {
			contextJSON, marshalErr := json.Marshal([]map[string]string{{
				"role":    "user",
				"content": fmt.Sprintf("[Recovered diagnostic turn] The application stopped before this diagnostic run completed. Original goal: %s. Continue from any evidence already captured for this run and do not assume the previous run reached a final answer.", run.Goal),
			}})
			if marshalErr != nil {
				return fmt.Errorf("encode recovered conversation context: %w", marshalErr)
			}
			if err = upsertConversationContext(tx, run.ConversationID, run.ID, contextJSON, endedAt); err != nil {
				return err
			}
		}
		event := newPersistedEvent(run, domain.EventRunFailed, map[string]any{"run": run, "error": runError})
		payload, marshalErr := json.Marshal(event.Payload)
		if marshalErr != nil {
			return fmt.Errorf("encode interrupted-run event: %w", marshalErr)
		}
		if err = insertEvent(tx, event, payload); err != nil {
			return err
		}
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("commit interrupted-run recovery: %w", err)
	}
	return nil
}

// CurrentConversation 返回指定档案当前 active 的会话；若不存在则新建。
func (j *Journal) CurrentConversation(profileID string) (domain.Conversation, error) {
	conversation, err := scanConversation(j.db.QueryRow(`SELECT id, profile_id, title, status, created_at, updated_at
        FROM conversations WHERE profile_id = ? AND status = 'active'`, profileID))
	if err == nil {
		return conversation, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return domain.Conversation{}, fmt.Errorf("read active conversation: %w", err)
	}
	return j.NewConversation(profileID)
}

// Conversations 返回指定档案的全部会话，按更新时间倒序排列。
func (j *Journal) Conversations(profileID string) ([]domain.Conversation, error) {
	rows, err := j.db.Query(`SELECT id, profile_id, title, status, created_at, updated_at
        FROM conversations WHERE profile_id = ? ORDER BY updated_at DESC`, profileID)
	if err != nil {
		return nil, fmt.Errorf("list conversations: %w", err)
	}
	defer rows.Close()
	conversations := make([]domain.Conversation, 0)
	for rows.Next() {
		conversation, scanErr := scanConversation(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		conversations = append(conversations, conversation)
	}
	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate conversations: %w", err)
	}
	return conversations, nil
}

// ActivateConversation 切换指定档案的 active 会话：归档当前会话并激活目标会话。
func (j *Journal) ActivateConversation(profileID, conversationID string) (domain.Conversation, error) {
	now := time.Now().UTC()
	tx, err := j.db.Begin()
	if err != nil {
		return domain.Conversation{}, fmt.Errorf("begin conversation switch: %w", err)
	}
	defer tx.Rollback()
	var selectedProfileID string
	if err = tx.QueryRow("SELECT profile_id FROM conversations WHERE id = ?", conversationID).Scan(&selectedProfileID); errors.Is(err, sql.ErrNoRows) {
		return domain.Conversation{}, errors.New("conversation not found")
	} else if err != nil {
		return domain.Conversation{}, fmt.Errorf("read conversation for switch: %w", err)
	} else if selectedProfileID != profileID {
		return domain.Conversation{}, errors.New("conversation belongs to another target profile")
	}
	if _, err = tx.Exec("UPDATE conversations SET status = 'archived', updated_at = ? WHERE profile_id = ? AND status = 'active' AND id <> ?", databaseTime(now), profileID, conversationID); err != nil {
		return domain.Conversation{}, fmt.Errorf("archive current conversation: %w", err)
	}
	if _, err = tx.Exec("UPDATE conversations SET status = 'active', updated_at = ? WHERE id = ?", databaseTime(now), conversationID); err != nil {
		return domain.Conversation{}, fmt.Errorf("activate conversation: %w", err)
	}
	if err = tx.Commit(); err != nil {
		return domain.Conversation{}, fmt.Errorf("commit conversation switch: %w", err)
	}
	return j.CurrentConversation(profileID)
}

// NewConversation 将档案现有 active 会话归档，并创建一个新会话。
func (j *Journal) NewConversation(profileID string) (domain.Conversation, error) {
	now := time.Now().UTC()
	conversation := domain.Conversation{
		ID:        uuid.NewString(),
		ProfileID: profileID,
		Status:    domain.ConversationActive,
		CreatedAt: now,
		UpdatedAt: now,
	}
	tx, err := j.db.Begin()
	if err != nil {
		return domain.Conversation{}, fmt.Errorf("begin new conversation: %w", err)
	}
	defer tx.Rollback()
	if _, err = tx.Exec("UPDATE conversations SET status = 'archived', updated_at = ? WHERE profile_id = ? AND status = 'active'", databaseTime(now), profileID); err != nil {
		return domain.Conversation{}, fmt.Errorf("archive active conversation: %w", err)
	}
	if _, err = tx.Exec(`INSERT INTO conversations(id, profile_id, title, status, created_at, updated_at)
        VALUES(?, ?, '', 'active', ?, ?)`, conversation.ID, profileID, databaseTime(now), databaseTime(now)); err != nil {
		return domain.Conversation{}, fmt.Errorf("create conversation: %w", err)
	}
	if err = tx.Commit(); err != nil {
		return domain.Conversation{}, fmt.Errorf("commit new conversation: %w", err)
	}
	return conversation, nil
}

// StartRun 在单个事务中写入 run 记录、更新会话标题并追加启动事件。
func (j *Journal) StartRun(run domain.Run, event domain.Event) error {
	payload, err := json.Marshal(event.Payload)
	if err != nil {
		return fmt.Errorf("encode run-started event: %w", err)
	}
	tx, err := j.db.Begin()
	if err != nil {
		return fmt.Errorf("begin diagnostic run: %w", err)
	}
	defer tx.Rollback()
	if _, err = tx.Exec(`INSERT INTO runs(id, conversation_id, profile_id, goal, mode, status, started_at)
	        VALUES(?, ?, ?, ?, ?, ?, ?)`, run.ID, run.ConversationID, run.ProfileID, run.Goal, run.Mode, run.Status, databaseTime(run.StartedAt)); err != nil {
		return fmt.Errorf("store diagnostic run: %w", err)
	}
	if _, err = tx.Exec(`UPDATE conversations SET title = CASE WHEN title = '' THEN ? ELSE title END, updated_at = ? WHERE id = ?`, run.Goal, databaseTime(run.StartedAt), run.ConversationID); err != nil {
		return fmt.Errorf("update conversation for diagnostic run: %w", err)
	}
	if err = insertEvent(tx, event, payload); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("commit diagnostic run: %w", err)
	}
	return nil
}

// FinishRun 更新 run 的终态与结束时间，保存最终上下文快照，并追加终态事件。
func (j *Journal) FinishRun(run domain.Run, runError string, contextJSON []byte, event domain.Event) error {
	payload, err := json.Marshal(event.Payload)
	if err != nil {
		return fmt.Errorf("encode run-finished event: %w", err)
	}
	tx, err := j.db.Begin()
	if err != nil {
		return fmt.Errorf("begin finishing diagnostic run: %w", err)
	}
	defer tx.Rollback()
	var endedAt any
	if run.EndedAt != nil {
		endedAt = databaseTime(*run.EndedAt)
	}
	if _, err = tx.Exec("UPDATE runs SET status = ?, ended_at = ?, error = ? WHERE id = ?", run.Status, endedAt, runError, run.ID); err != nil {
		return fmt.Errorf("finish diagnostic run: %w", err)
	}
	if len(contextJSON) > 0 {
		if err = upsertConversationContext(tx, run.ConversationID, run.ID, contextJSON, time.Now().UTC()); err != nil {
			return err
		}
	}
	if _, err = tx.Exec("UPDATE conversations SET updated_at = ? WHERE id = ?", databaseTime(time.Now().UTC()), run.ConversationID); err != nil {
		return fmt.Errorf("update conversation completion time: %w", err)
	}
	if err = insertEvent(tx, event, payload); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("commit diagnostic run completion: %w", err)
	}
	return nil
}

// SaveContext 在 run 仍运行期间保存可恢复的 Eino 消息边界。
// run 状态与终态事件仍由 FinishRun 负责；本方法只推进会话快照，
// 使应用重启时不会丢失已完成的诊断轮次。
func (j *Journal) SaveContext(conversationID, sourceRunID string, contextJSON []byte) error {
	if len(contextJSON) == 0 {
		return nil
	}
	tx, err := j.db.Begin()
	if err != nil {
		return fmt.Errorf("begin conversation context checkpoint: %w", err)
	}
	defer tx.Rollback()
	if err = upsertConversationContext(tx, conversationID, sourceRunID, contextJSON, time.Now().UTC()); err != nil {
		return err
	}
	if _, err = tx.Exec("UPDATE conversations SET updated_at = ? WHERE id = ?", databaseTime(time.Now().UTC()), conversationID); err != nil {
		return fmt.Errorf("update conversation checkpoint time: %w", err)
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("commit conversation context checkpoint: %w", err)
	}
	return nil
}

// upsertConversationContext 以"插入或 revision+1 更新"的方式保存会话上下文快照。
func upsertConversationContext(tx *sql.Tx, conversationID, sourceRunID string, contextJSON []byte, updatedAt time.Time) error {
	if _, err := tx.Exec(`INSERT INTO conversation_contexts(conversation_id, format_version, revision, source_run_id, messages_json, updated_at)
        VALUES(?, 1, 1, ?, ?, ?)
        ON CONFLICT(conversation_id) DO UPDATE SET
            format_version = excluded.format_version,
            revision = conversation_contexts.revision + 1,
            source_run_id = excluded.source_run_id,
            messages_json = excluded.messages_json,
            updated_at = excluded.updated_at`, conversationID, sourceRunID, contextJSON, databaseTime(updatedAt)); err != nil {
		return fmt.Errorf("store conversation context: %w", err)
	}
	return nil
}

// Append 向 events 表追加一条诊断事件。
func (j *Journal) Append(event domain.Event) error {
	payload, err := json.Marshal(event.Payload)
	if err != nil {
		return fmt.Errorf("encode diagnostic event: %w", err)
	}
	if _, err = j.db.Exec(`INSERT INTO events(id, conversation_id, run_id, type, timestamp, payload_json)
        VALUES(?, ?, ?, ?, ?, ?)`, event.ID, event.ConversationID, event.RunID, event.Type, databaseTime(event.Timestamp), payload); err != nil {
		return fmt.Errorf("store diagnostic event: %w", err)
	}
	return nil
}

// Events 返回指定会话的全部事件，按写入顺序排列。
func (j *Journal) Events(conversationID string) ([]domain.Event, error) {
	rows, err := j.db.Query(`SELECT id, conversation_id, run_id, type, timestamp, payload_json
        FROM events WHERE conversation_id = ? ORDER BY sequence`, conversationID)
	if err != nil {
		return nil, fmt.Errorf("read conversation events: %w", err)
	}
	defer rows.Close()

	events := make([]domain.Event, 0)
	for rows.Next() {
		var event domain.Event
		var timestamp string
		var payload []byte
		if err = rows.Scan(&event.ID, &event.ConversationID, &event.RunID, &event.Type, &timestamp, &payload); err != nil {
			return nil, fmt.Errorf("scan conversation event: %w", err)
		}
		if event.Timestamp, err = parseDatabaseTime(timestamp); err != nil {
			return nil, err
		}
		if err = json.Unmarshal(payload, &event.Payload); err != nil {
			return nil, fmt.Errorf("decode diagnostic event %s: %w", event.ID, err)
		}
		events = append(events, event)
	}
	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate conversation events: %w", err)
	}
	return events, nil
}

// Context 返回指定会话最近保存的上下文快照，不存在时返回 found=false。
func (j *Journal) Context(conversationID string) ([]byte, bool, error) {
	var content []byte
	err := j.db.QueryRow("SELECT messages_json FROM conversation_contexts WHERE conversation_id = ?", conversationID).Scan(&content)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("read conversation context: %w", err)
	}
	return content, true, nil
}

func (j *Journal) SaveContextFact(fact domain.ContextFact) error {
	if strings.TrimSpace(fact.ConversationID) == "" {
		return errors.New("context fact conversation ID is required")
	}
	if strings.TrimSpace(fact.Key) == "" {
		return errors.New("context fact key is required")
	}
	now := time.Now().UTC()
	if fact.FirstSeen.IsZero() {
		fact.FirstSeen = now
	}
	if fact.LastSeen.IsZero() {
		fact.LastSeen = now
	}
	evidenceIDs, err := json.Marshal(fact.EvidenceIDs)
	if err != nil {
		return fmt.Errorf("encode context fact evidence IDs: %w", err)
	}
	_, err = j.db.Exec(`INSERT INTO context_facts(
        conversation_id, fact_key, kind, status, content_json, evidence_ids_json,
        source_tool, source_locator, confidence, first_seen, last_seen, revision)
        VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 1)
        ON CONFLICT(conversation_id, fact_key) DO UPDATE SET
            kind = excluded.kind,
            status = excluded.status,
            content_json = excluded.content_json,
            evidence_ids_json = excluded.evidence_ids_json,
            source_tool = excluded.source_tool,
            source_locator = excluded.source_locator,
            confidence = excluded.confidence,
            first_seen = context_facts.first_seen,
            last_seen = excluded.last_seen,
            revision = context_facts.revision + 1`,
		fact.ConversationID, fact.Key, fact.Kind, fact.Status, []byte(fact.Content), evidenceIDs,
		fact.SourceTool, fact.SourceLocator, fact.Confidence, databaseTime(fact.FirstSeen), databaseTime(fact.LastSeen))
	if err != nil {
		return fmt.Errorf("store context fact: %w", err)
	}
	return nil
}

func (j *Journal) ContextFacts(conversationID string, limit int) ([]domain.ContextFact, error) {
	if limit <= 0 {
		limit = 64
	}
	if limit > 256 {
		limit = 256
	}
	rows, err := j.db.Query(`SELECT fact_key, kind, status, content_json,
        evidence_ids_json, source_tool, source_locator, confidence,
        first_seen, last_seen, revision
        FROM context_facts WHERE conversation_id = ?
        ORDER BY last_seen DESC LIMIT ?`, conversationID, limit)
	if err != nil {
		return nil, fmt.Errorf("read context facts: %w", err)
	}
	defer rows.Close()
	facts := make([]domain.ContextFact, 0)
	for rows.Next() {
		var fact domain.ContextFact
		var evidenceIDs []byte
		var firstSeen, lastSeen string
		if err = rows.Scan(&fact.Key, &fact.Kind, &fact.Status, &fact.Content,
			&evidenceIDs, &fact.SourceTool, &fact.SourceLocator, &fact.Confidence,
			&firstSeen, &lastSeen, &fact.Revision); err != nil {
			return nil, fmt.Errorf("scan context fact: %w", err)
		}
		fact.ConversationID = conversationID
		if err = json.Unmarshal(evidenceIDs, &fact.EvidenceIDs); err != nil {
			return nil, fmt.Errorf("decode context fact evidence IDs: %w", err)
		}
		if fact.FirstSeen, err = parseDatabaseTime(firstSeen); err != nil {
			return nil, err
		}
		if fact.LastSeen, err = parseDatabaseTime(lastSeen); err != nil {
			return nil, err
		}
		facts = append(facts, fact)
	}
	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate context facts: %w", err)
	}
	return facts, nil
}

func (j *Journal) SavePlan(plan domain.DiagnosticPlan) error {
	if strings.TrimSpace(plan.ConversationID) == "" {
		return errors.New("diagnostic plan conversation ID is required")
	}
	if plan.UpdatedAt.IsZero() {
		plan.UpdatedAt = time.Now().UTC()
	}
	payload, err := json.Marshal(plan)
	if err != nil {
		return fmt.Errorf("encode diagnostic plan: %w", err)
	}
	_, err = j.db.Exec(`INSERT INTO context_plans(conversation_id, plan_json, revision, updated_at)
        VALUES(?, ?, 1, ?)
        ON CONFLICT(conversation_id) DO UPDATE SET
            plan_json = excluded.plan_json,
            revision = context_plans.revision + 1,
            updated_at = excluded.updated_at`, plan.ConversationID, payload, databaseTime(plan.UpdatedAt))
	if err != nil {
		return fmt.Errorf("store diagnostic plan: %w", err)
	}
	return nil
}

func (j *Journal) Plan(conversationID string) (domain.DiagnosticPlan, bool, error) {
	var payload []byte
	var revision int
	err := j.db.QueryRow("SELECT plan_json, revision FROM context_plans WHERE conversation_id = ?", conversationID).Scan(&payload, &revision)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.DiagnosticPlan{}, false, nil
	}
	if err != nil {
		return domain.DiagnosticPlan{}, false, fmt.Errorf("read diagnostic plan: %w", err)
	}
	var plan domain.DiagnosticPlan
	if err = json.Unmarshal(payload, &plan); err != nil {
		return domain.DiagnosticPlan{}, false, fmt.Errorf("decode diagnostic plan: %w", err)
	}
	plan.Revision = revision
	return plan, true, nil
}

func (j *Journal) SaveToolFingerprint(record domain.ToolFingerprint) error {
	if strings.TrimSpace(record.ConversationID) == "" || strings.TrimSpace(record.Fingerprint) == "" {
		return errors.New("tool fingerprint conversation ID and fingerprint are required")
	}
	if record.LastSeen.IsZero() {
		record.LastSeen = time.Now().UTC()
	}
	_, err := j.db.Exec(`INSERT INTO tool_fingerprints(
        conversation_id, fingerprint, tool_name, evidence_id, successful, last_seen)
        VALUES(?, ?, ?, ?, ?, ?)
        ON CONFLICT(conversation_id, fingerprint) DO UPDATE SET
            tool_name = excluded.tool_name,
            evidence_id = excluded.evidence_id,
            successful = excluded.successful,
            last_seen = excluded.last_seen`, record.ConversationID, record.Fingerprint,
		record.ToolName, record.EvidenceID, boolInt(record.Successful), databaseTime(record.LastSeen))
	if err != nil {
		return fmt.Errorf("store tool fingerprint: %w", err)
	}
	return nil
}

func (j *Journal) ToolFingerprint(conversationID, fingerprint string) (domain.ToolFingerprint, bool, error) {
	var record domain.ToolFingerprint
	var successful int
	var lastSeen string
	err := j.db.QueryRow(`SELECT fingerprint, tool_name, evidence_id, successful, last_seen
        FROM tool_fingerprints WHERE conversation_id = ? AND fingerprint = ?`, conversationID, fingerprint).
		Scan(&record.Fingerprint, &record.ToolName, &record.EvidenceID, &successful, &lastSeen)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.ToolFingerprint{}, false, nil
	}
	if err != nil {
		return domain.ToolFingerprint{}, false, fmt.Errorf("read tool fingerprint: %w", err)
	}
	record.ConversationID = conversationID
	record.Successful = successful != 0
	if record.LastSeen, err = parseDatabaseTime(lastSeen); err != nil {
		return domain.ToolFingerprint{}, false, err
	}
	return record, true, nil
}

// DeleteToolFingerprintsByConversation 删除指定 conversation 的所有 tool fingerprint 记录。
// 在写入工具（POST/PUT/DELETE/edit-config 等）成功后调用，防止后续只读 GET 命中
// 写入前的 SQLite 缓存，导致模型看到过期的设备状态。
func (j *Journal) DeleteToolFingerprintsByConversation(conversationID string) error {
	_, err := j.db.Exec(`DELETE FROM tool_fingerprints WHERE conversation_id = ?`, conversationID)
	if err != nil {
		return fmt.Errorf("delete tool fingerprints: %w", err)
	}
	return nil
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

// Evidence returns the original evidence captured by a run. The raw payload
// is kept in the event journal and is intentionally loaded only when the
// diagnostic agent explicitly asks for a specific evidence ID.
func (j *Journal) Evidence(runID, evidenceID string) (domain.Evidence, bool, error) {
	rows, err := j.db.Query(`SELECT payload_json FROM events
        WHERE run_id = ? AND type = ? ORDER BY sequence`, runID, domain.EventEvidenceCaptured)
	if err != nil {
		return domain.Evidence{}, false, fmt.Errorf("read evidence events: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var payload []byte
		if err = rows.Scan(&payload); err != nil {
			return domain.Evidence{}, false, fmt.Errorf("scan evidence event: %w", err)
		}
		var evidence domain.Evidence
		if err = json.Unmarshal(payload, &evidence); err != nil {
			return domain.Evidence{}, false, fmt.Errorf("decode evidence event: %w", err)
		}
		if evidence.ID == evidenceID {
			return evidence, true, nil
		}
	}
	if err = rows.Err(); err != nil {
		return domain.Evidence{}, false, fmt.Errorf("iterate evidence events: %w", err)
	}
	return domain.Evidence{}, false, nil
}

// EvidenceByConversation 按 conversation_id + evidence_id 拿回旧 evidence。
// 用于跨 Run 但同 conversation 的工具结果复用：Runner.Execute 命中 SQLite 里
// tool_fingerprints 表的旧记录后，通过这条路径拿回上一次的只读结果，
// 避免重新发 NBI/NETCONF 请求或重新扫文件。
//
// 注意：events 表上 conversation_id + type 上目前没有复合索引，但是同一会话的
// evidence.captured 事件数量通常很小（几十到几百），线性扫描可接受。
// 如果未来发现性能问题，可以加 CREATE INDEX events_by_conversation_type
// ON events(conversation_id, type, sequence)。
func (j *Journal) EvidenceByConversation(conversationID, evidenceID string) (domain.Evidence, bool, error) {
	rows, err := j.db.Query(`SELECT payload_json FROM events
        WHERE conversation_id = ? AND type = ? ORDER BY sequence`, conversationID, domain.EventEvidenceCaptured)
	if err != nil {
		return domain.Evidence{}, false, fmt.Errorf("read conversation evidence events: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var payload []byte
		if err = rows.Scan(&payload); err != nil {
			return domain.Evidence{}, false, fmt.Errorf("scan conversation evidence event: %w", err)
		}
		var evidence domain.Evidence
		if err = json.Unmarshal(payload, &evidence); err != nil {
			return domain.Evidence{}, false, fmt.Errorf("decode conversation evidence event: %w", err)
		}
		if evidence.ID == evidenceID {
			return evidence, true, nil
		}
	}
	if err = rows.Err(); err != nil {
		return domain.Evidence{}, false, fmt.Errorf("iterate conversation evidence events: %w", err)
	}
	return domain.Evidence{}, false, nil
}

// SaveTokenUsage persists one model-generation usage record. Each ChatModel
// generation inside a run appends one row; downstream callers can sum per run
// or per conversation to show real token cost and latency instead of estimates.
func (j *Journal) SaveTokenUsage(record domain.TokenUsageRecord) error {
	if strings.TrimSpace(record.ID) == "" {
		return errors.New("token usage record ID is required")
	}
	if strings.TrimSpace(record.RunID) == "" || strings.TrimSpace(record.ConversationID) == "" {
		return errors.New("token usage record run and conversation IDs are required")
	}
	if record.RecordedAt.IsZero() {
		record.RecordedAt = time.Now().UTC()
	}
	_, err := j.db.Exec(`INSERT INTO token_usage(
        id, run_id, conversation_id, iteration, input_tokens, output_tokens, total_tokens, elapsed_ms, recorded_at)
        VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		record.ID, record.RunID, record.ConversationID, record.Iteration,
		record.InputTokens, record.OutputTokens, record.TotalTokens, record.ElapsedMS,
		databaseTime(record.RecordedAt))
	if err != nil {
		return fmt.Errorf("store token usage: %w", err)
	}
	return nil
}

// TokenUsageByConversation returns token usage rows for a conversation in
// model-generation order. Used to display per-turn cost/latency after a run.
func (j *Journal) TokenUsageByConversation(conversationID string) ([]domain.TokenUsageRecord, error) {
	rows, err := j.db.Query(`SELECT id, run_id, conversation_id, iteration, input_tokens, output_tokens, total_tokens, elapsed_ms, recorded_at
        FROM token_usage WHERE conversation_id = ? ORDER BY recorded_at, iteration`, conversationID)
	if err != nil {
		return nil, fmt.Errorf("read token usage: %w", err)
	}
	defer rows.Close()
	records := make([]domain.TokenUsageRecord, 0)
	for rows.Next() {
		var record domain.TokenUsageRecord
		var recordedAt string
		if err = rows.Scan(&record.ID, &record.RunID, &record.ConversationID, &record.Iteration,
			&record.InputTokens, &record.OutputTokens, &record.TotalTokens, &record.ElapsedMS, &recordedAt); err != nil {
			return nil, fmt.Errorf("scan token usage: %w", err)
		}
		if record.RecordedAt, err = parseDatabaseTime(recordedAt); err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate token usage: %w", err)
	}
	return records, nil
}

func insertEvent(tx *sql.Tx, event domain.Event, payload []byte) error {
	if _, err := tx.Exec(`INSERT INTO events(id, conversation_id, run_id, type, timestamp, payload_json)
        VALUES(?, ?, ?, ?, ?, ?)`, event.ID, event.ConversationID, event.RunID, event.Type, databaseTime(event.Timestamp), payload); err != nil {
		return fmt.Errorf("store diagnostic event: %w", err)
	}
	return nil
}

func newPersistedEvent(run domain.Run, eventType domain.EventType, payload any) domain.Event {
	return domain.Event{
		ID:             uuid.NewString(),
		ConversationID: run.ConversationID,
		RunID:          run.ID,
		Type:           eventType,
		Timestamp:      time.Now().UTC(),
		Payload:        payload,
	}
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanConversation(row rowScanner) (domain.Conversation, error) {
	var conversation domain.Conversation
	var createdAt string
	var updatedAt string
	if err := row.Scan(&conversation.ID, &conversation.ProfileID, &conversation.Title, &conversation.Status, &createdAt, &updatedAt); err != nil {
		return domain.Conversation{}, err
	}
	var err error
	if conversation.CreatedAt, err = parseDatabaseTime(createdAt); err != nil {
		return domain.Conversation{}, err
	}
	if conversation.UpdatedAt, err = parseDatabaseTime(updatedAt); err != nil {
		return domain.Conversation{}, err
	}
	return conversation, nil
}

func databaseTime(value time.Time) string {
	return value.UTC().Format(time.RFC3339Nano)
}

func parseDatabaseTime(value string) (time.Time, error) {
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse database timestamp %q: %w", value, err)
	}
	return parsed, nil
}

type ApprovalBroker struct {
	mu                    sync.Mutex
	pending               map[string]pendingApproval
	approvedConversations map[string]struct{}
}

type pendingApproval struct {
	conversationID string
	result         chan bool
}

func NewApprovalBroker() *ApprovalBroker {
	return &ApprovalBroker{
		pending:               make(map[string]pendingApproval),
		approvedConversations: make(map[string]struct{}),
	}
}

func (b *ApprovalBroker) Open(request domain.ApprovalRequest) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, exists := b.pending[request.CallID]; exists {
		return fmt.Errorf("approval already pending for call: %s", request.CallID)
	}
	b.pending[request.CallID] = pendingApproval{
		conversationID: request.ConversationID,
		result:         make(chan bool, 1),
	}
	return nil
}

func (b *ApprovalBroker) Wait(ctx context.Context, callID string) (bool, error) {
	b.mu.Lock()
	pending, exists := b.pending[callID]
	b.mu.Unlock()
	if !exists {
		return false, fmt.Errorf("approval not found for call: %s", callID)
	}

	select {
	case approved := <-pending.result:
		return approved, nil
	case <-ctx.Done():
		b.mu.Lock()
		delete(b.pending, callID)
		b.mu.Unlock()
		return false, ctx.Err()
	}
}

func (b *ApprovalBroker) Resolve(callID string, approved bool, approveConversation bool) error {
	b.mu.Lock()
	pending, exists := b.pending[callID]
	if exists {
		delete(b.pending, callID)
		if approved && approveConversation {
			b.approvedConversations[pending.conversationID] = struct{}{}
		}
	}
	b.mu.Unlock()
	if !exists {
		return errors.New("approval request is not pending")
	}
	pending.result <- approved
	close(pending.result)
	return nil
}

func (b *ApprovalBroker) ConversationApproved(conversationID string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	_, approved := b.approvedConversations[conversationID]
	return approved
}

func (b *ApprovalBroker) ClearConversation(conversationID string) {
	b.mu.Lock()
	delete(b.approvedConversations, conversationID)
	b.mu.Unlock()
}
