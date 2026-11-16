// gsm_import.go holds gsmImportSource.

package mirror

import _ "embed"

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"
)

// This file reads the subscriber registry of github-state-mirror, the service this engine replaces, and nothing else.

// gsmImportSource keys the import ledger. A moved file keeps its ledger.
const gsmImportSource = "github-state-mirror"

// gsmSelect reads every column of github-state-mirror's subscriptions table.
//
//go:embed gsm-select.sql
var gsmSelect string

// gsmFingerprint is the principal github-state-mirror gives a credential it cannot name: the hex SHA-256 of the bare token.
var gsmFingerprint = regexp.MustCompile(`^[0-9a-f]{64}$`)

// ImportReport is what one startup import did.
type ImportReport struct {
	Source   string   `json:"source"`
	File     string   `json:"file"`
	Imported int      `json:"imported"`
	Present  int      `json:"already_present"`
	Failed   int      `json:"failed"`
	Failures []string `json:"failures,omitempty"`
}

// importGSMSubscriptions adopts every registration of the named file that
// the ledger has not recorded.
//
// A file that is missing, unreadable, or not this format is an error. A row
// that cannot be translated is counted, logged with its id, and kept out.
// It is tried again on the next start, so a spec that gains the identity the
// row needs adopts it then.
func importGSMSubscriptions(ctx context.Context, spec *Spec, subs *Subscriptions, path string) (ImportReport, error) {
	report := ImportReport{Source: gsmImportSource, File: path}
	src, err := os.Stat(path)
	if err != nil {
		return report, fmt.Errorf("<notify> import: %w", err)
	}
	if own, err := os.Stat(subs.Path()); err == nil && os.SameFile(src, own) {
		return report, fmt.Errorf("<notify> import %q is this mirror's own subscription file", path)
	}
	// Read-only: the import must never change the file it reads.
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		return report, fmt.Errorf("<notify> import %q: %w", path, err)
	}
	defer db.Close()

	rows, err := readGSMRows(ctx, db)
	if err != nil {
		return report, fmt.Errorf("<notify> import %q is not a github-state-mirror subscription registry: %w", path, err)
	}
	for _, row := range rows {
		sub, err := row.translate(spec)
		if err != nil {
			report.fail(row.id, err)
			continue
		}
		outcome, err := subs.adopt(ctx, gsmImportSource, row.id, sub)
		if err != nil {
			return report, fmt.Errorf("<notify> import %q: %w", path, err)
		}
		switch outcome {
		case adoptImported:
			report.Imported++
		case adoptPresent:
			report.Present++
		case adoptIDTaken:
			report.fail(row.id, errors.New("a subscription with this id already exists here"))
		}
	}
	summary := fmt.Sprintf("notify: import from %s: %d imported, %d already present, %d failed",
		path, report.Imported, report.Present, report.Failed)
	if report.Failed > 0 {
		logf("%s", summary)
	} else {
		logger.Info(summary)
	}
	return report, nil
}

func (r *ImportReport) fail(id string, err error) {
	r.Failed++
	r.Failures = append(r.Failures, id+": "+err.Error())
	logf("notify: import row %s NOT imported: %v", id, err)
}

// gsmRow is one row as github-state-mirror stores it.
type gsmRow struct {
	id, principal, url, secret        string
	repoFilters, eventFilters         string
	active, failures                  int64
	disabledReason                    string
	createdAt, updatedAt              string
	lastSuccess, lastFailure, lastErr string
}

func readGSMRows(ctx context.Context, db *sql.DB) ([]gsmRow, error) {
	rows, err := db.QueryContext(ctx, gsmSelect)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []gsmRow
	for rows.Next() {
		var g gsmRow
		if err := rows.Scan(&g.id, &g.principal, &g.url, &g.secret, &g.repoFilters, &g.eventFilters,
			&g.active, &g.failures, &g.disabledReason, &g.createdAt, &g.updatedAt,
			&g.lastSuccess, &g.lastFailure, &g.lastErr); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// translate maps one row to a subscription of this engine, or says why it
// cannot.
func (g gsmRow) translate(spec *Spec) (Subscription, error) {
	principal, err := gsmPrincipal(spec, g.principal)
	if err != nil {
		return Subscription{}, err
	}
	if err := checkCallbackURL(g.url); err != nil {
		return Subscription{}, fmt.Errorf("url: %w", err)
	}
	if len(g.secret) < minCallerSecret {
		return Subscription{}, fmt.Errorf("secret is shorter than %d characters", minCallerSecret)
	}
	scopes, err := gsmFilters("repo_filters", g.repoFilters)
	if err != nil {
		return Subscription{}, err
	}
	events, err := gsmFilters("event_filters", g.eventFilters)
	if err != nil {
		return Subscription{}, err
	}
	sub := Subscription{
		ID: g.id, Principal: principal, URL: g.url, Secret: g.secret,
		Events: events, Scopes: scopes,
		Failures: int(g.failures), Disabled: g.active == 0,
		LastError: g.lastErr, DisabledReason: g.disabledReason,
	}
	for _, ts := range []struct {
		name, value string
		required    bool
		dst         *time.Time
	}{
		{"created_at", g.createdAt, true, &sub.CreatedAt},
		{"updated_at", g.updatedAt, true, &sub.UpdatedAt},
		{"last_success_at", g.lastSuccess, false, &sub.LastOK},
		{"last_failure_at", g.lastFailure, false, &sub.LastFailure},
	} {
		if ts.value == "" {
			if ts.required {
				return Subscription{}, fmt.Errorf("%s is empty", ts.name)
			}
			continue
		}
		t, err := time.Parse(time.RFC3339Nano, ts.value)
		if err != nil {
			return Subscription{}, fmt.Errorf("%s: %w", ts.name, err)
		}
		*ts.dst = t.UTC()
	}
	return sub, nil
}

// gsmFilters decodes a JSON filter list. A list that does not decode is an
// error: reading it as empty would widen the filter to everything.
func gsmFilters(column, raw string) ([]string, error) {
	var list []string
	if err := json.Unmarshal([]byte(raw), &list); err != nil {
		return nil, fmt.Errorf("%s %q is not a JSON list of strings", column, raw)
	}
	for _, v := range list {
		// This store joins a list with commas.
		if v == "" || strings.Contains(v, ",") {
			return nil, fmt.Errorf("%s entry %q cannot be stored", column, v)
		}
	}
	if len(list) == 0 {
		return nil, nil
	}
	return list, nil
}

// gsmPrincipal maps a github-state-mirror principal to the string this spec
// resolves for the same caller. A principal no caller here can resolve to is
// an error, because its subscriber could never manage the row.
//
//   - a bare 64-hex fingerprint is token:<fingerprint>, the digest tokenPrincipal computes.
//   - user:<id> is the owner GET /user names, so it needs <whoami prefix="user" path="/user" id="id">.
//   - app:<id> is a verified App assertion, so it needs <identity prefix="app" path="/app" id="id">.
func gsmPrincipal(spec *Spec, p string) (string, error) {
	if gsmFingerprint.MatchString(p) {
		return "token:" + p, nil
	}
	prefix, id, ok := strings.Cut(p, ":")
	if !ok || id == "" {
		return "", fmt.Errorf("principal %q is not a form github-state-mirror writes", p)
	}
	switch prefix {
	case "user":
		w := spec.Whoami
		if w == nil || w.Prefix != "user" || w.Path != "/user" || w.ID != "id" {
			return "", fmt.Errorf(`principal %q needs <whoami prefix="user" path="/user" id="id">, which this spec does not declare`, p)
		}
	case "app":
		i := spec.Identity
		if i == nil || i.Prefix != "app" || i.Path != "/app" || i.ID != "id" {
			return "", fmt.Errorf(`principal %q needs <identity prefix="app" path="/app" id="id">, which this spec does not declare`, p)
		}
	default:
		return "", fmt.Errorf("principal %q: no caller of this mirror resolves to a %q principal", p, prefix)
	}
	return p, nil
}
