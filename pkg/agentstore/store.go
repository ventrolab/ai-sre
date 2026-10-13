// Package agentstore keeps every agent that ever connected to the Tool Hub in PostgreSQL,
// so the web console can show offline agents, versions and credentials; it also keeps
// join tokens, issued agent tokens and certificates.
//
// The hub owns the table: it records connects and disconnects and refreshes last_seen_at
// for connected agents. Only one hub replica may write (the chart runs one); at startup
// the hub marks every agent offline and the connected ones come back within seconds.
package agentstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const schema = `
create table if not exists agents (
    name text primary key,
    kind text not null default '',
    labels jsonb not null default '{}',
    tools jsonb not null default '[]',
    version text,
    remote text,
    cert_serial text,
    cert_not_after timestamptz,
    online boolean not null default false,
    first_seen_at timestamptz not null default now(),
    connected_at timestamptz,
    last_seen_at timestamptz,
    disconnected_at timestamptz
);

-- One-time join tokens, created by the web console (UI or API), redeemed by the hub.
create table if not exists agent_enrollments (
    token_hash text primary key,          -- hex SHA-256 of the token
    name text not null,
    note text,
    created_by text not null,
    created_at timestamptz not null default now(),
    expires_at timestamptz not null,
    used_at timestamptz,
    used_from text,
    cert_serial text
);

-- Certificates issued by enrollment, and revoked serials (also of hand-made certificates).
create table if not exists agent_certs (
    serial text primary key,              -- hex, as in agents.cert_serial
    name text not null,
    not_after timestamptz,
    source text not null,                 -- enroll | external
    issued_at timestamptz not null default now(),
    revoked_at timestamptz,
    revoked_by text
);

-- Agent tokens issued by enrollment (bearer credentials of the tunnel). Only the hash is
-- stored; id is its public prefix, shown in the console and logs.
create table if not exists agent_tokens (
    id text primary key,
    token_hash text not null unique,
    name text not null,
    issued_at timestamptz not null default now(),
    last_used_at timestamptz,
    revoked_at timestamptz,
    revoked_by text
);
create index if not exists agent_tokens_name on agent_tokens (name) where revoked_at is null;

alter table agents add column if not exists auth text;
alter table agents add column if not exists token_id text;
alter table agent_enrollments add column if not exists token_id text;`

// Session is what the hub knows about a connected agent.
type Session struct {
	Name         string
	Kind         string
	Labels       map[string]string
	Tools        []string
	Version      string
	Remote       string
	Auth         string
	CertSerial   string
	CertNotAfter time.Time
	TokenID      string
	ConnectedAt  time.Time
}

type Store struct {
	pool *pgxpool.Pool
}

// Open connects, creates the table and marks all agents offline (hub restart).
func Open(ctx context.Context, url string) (*Store, error) {
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, err
	}
	s := &Store{pool: pool}
	if _, err := pool.Exec(ctx, schema); err != nil {
		pool.Close()
		return nil, fmt.Errorf("create agents table: %w", err)
	}
	if _, err := pool.Exec(ctx, `update agents set online = false, disconnected_at = coalesce(last_seen_at, now())
		where online`); err != nil {
		pool.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() { s.pool.Close() }

func (s *Store) Connected(ctx context.Context, a Session) error {
	labels, _ := json.Marshal(nonNil(a.Labels))
	tools, _ := json.Marshal(append([]string{}, a.Tools...))
	var notAfter *time.Time
	if !a.CertNotAfter.IsZero() {
		notAfter = &a.CertNotAfter
	}
	_, err := s.pool.Exec(ctx, `
		insert into agents (name, kind, labels, tools, version, remote, auth, cert_serial, cert_not_after,
		                    token_id, online, connected_at, last_seen_at)
		values ($1, $2, $3, $4, $5, $6, $7, nullif($8, ''), $9, nullif($10, ''), true, $11, now())
		on conflict (name) do update set kind = excluded.kind, labels = excluded.labels,
		    tools = excluded.tools, version = excluded.version, remote = excluded.remote, auth = excluded.auth,
		    cert_serial = excluded.cert_serial, cert_not_after = excluded.cert_not_after,
		    token_id = excluded.token_id, online = true, connected_at = excluded.connected_at, last_seen_at = now()`,
		a.Name, a.Kind, labels, tools, a.Version, a.Remote, a.Auth, a.CertSerial, notAfter, a.TokenID, a.ConnectedAt)
	if err != nil || a.TokenID == "" {
		return err
	}
	_, err = s.pool.Exec(ctx, `update agent_tokens set last_used_at = now() where id = $1`, a.TokenID)
	return err
}

func (s *Store) Disconnected(ctx context.Context, name string) error {
	_, err := s.pool.Exec(ctx, `update agents set online = false, last_seen_at = now(), disconnected_at = now()
		where name = $1`, name)
	return err
}

// Heartbeat refreshes last_seen_at of the connected agents; the console treats an
// "online" agent without a recent heartbeat as stale (the hub itself is gone).
func (s *Store) Heartbeat(ctx context.Context, names []string) error {
	if len(names) == 0 {
		return nil
	}
	_, err := s.pool.Exec(ctx, `update agents set last_seen_at = now() where name = any($1) and online`, names)
	return err
}

func nonNil(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	return m
}

// ErrInvalidToken covers unknown, used and expired join tokens alike.
var ErrInvalidToken = errors.New("invalid, used or expired join token")

// RedeemEnrollment marks a join token used and returns the agent name it was created for.
func (s *Store) RedeemEnrollment(ctx context.Context, tokenHash, from string) (string, error) {
	var name string
	err := s.pool.QueryRow(ctx, `update agent_enrollments set used_at = now(), used_from = $2
		where token_hash = $1 and used_at is null and expires_at > now() returning name`, tokenHash, from).Scan(&name)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrInvalidToken
	}
	return name, err
}

// RecordIssued stores a certificate issued for a redeemed token.
func (s *Store) RecordIssued(ctx context.Context, tokenHash, serial, name string, notAfter time.Time) error {
	if _, err := s.pool.Exec(ctx, `insert into agent_certs (serial, name, not_after, source) values ($1, $2, $3, 'enroll')`,
		serial, name, notAfter); err != nil {
		return err
	}
	_, err := s.pool.Exec(ctx, `update agent_enrollments set cert_serial = $2 where token_hash = $1`, tokenHash, serial)
	return err
}

// RevokedSerials lists every revoked certificate serial.
func (s *Store) RevokedSerials(ctx context.Context) (map[string]bool, error) {
	rows, err := s.pool.Query(ctx, `select serial from agent_certs where revoked_at is not null`)
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for rows.Next() {
		var serial string
		if err := rows.Scan(&serial); err != nil {
			return nil, err
		}
		out[serial] = true
	}
	return out, rows.Err()
}

// TokenID is the public id of an agent token: a prefix of its hash.
func TokenID(tokenHash string) string {
	if len(tokenHash) > 16 {
		return tokenHash[:16]
	}
	return tokenHash
}

// IssueToken stores a new agent token for a redeemed join token. Earlier tokens of the
// same name stop working: re-enrollment replaces the agent's credential.
func (s *Store) IssueToken(ctx context.Context, joinHash, name, tokenHash string) (string, error) {
	id := TokenID(tokenHash)
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `update agent_tokens set revoked_at = now(), revoked_by = 're-enrolled'
		where name = $1 and revoked_at is null`, name); err != nil {
		return "", err
	}
	if _, err := tx.Exec(ctx, `insert into agent_tokens (id, token_hash, name) values ($1, $2, $3)`,
		id, tokenHash, name); err != nil {
		return "", err
	}
	if _, err := tx.Exec(ctx, `update agent_enrollments set token_id = $2 where token_hash = $1`, joinHash, id); err != nil {
		return "", err
	}
	return id, tx.Commit(ctx)
}

// ErrUnknownToken: no active agent token with this hash.
var ErrUnknownToken = errors.New("unknown or revoked agent token")

// AgentForToken returns the agent name and token id of an active agent token.
func (s *Store) AgentForToken(ctx context.Context, tokenHash string) (name, id string, err error) {
	err = s.pool.QueryRow(ctx, `select name, id from agent_tokens where token_hash = $1 and revoked_at is null`,
		tokenHash).Scan(&name, &id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", ErrUnknownToken
	}
	return name, id, err
}

// TokenRef is an active agent token as the hub caches it.
type TokenRef struct{ Name, ID string }

// ActiveTokens lists every active agent token by hash.
func (s *Store) ActiveTokens(ctx context.Context) (map[string]TokenRef, error) {
	rows, err := s.pool.Query(ctx, `select token_hash, name, id from agent_tokens where revoked_at is null`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]TokenRef{}
	for rows.Next() {
		var hash string
		var ref TokenRef
		if err := rows.Scan(&hash, &ref.Name, &ref.ID); err != nil {
			return nil, err
		}
		out[hash] = ref
	}
	return out, rows.Err()
}
