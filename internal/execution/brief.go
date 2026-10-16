// Package execution implements typed execution-plane contracts and canonical
// ExecutionBrief digests.
package execution

import (
	"bytes"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"unicode/utf8"

	jsoncanonicalizer "github.com/cyberphone/json-canonicalization/go/src/webpki.org/jsoncanonicalizer"
	"golang.org/x/text/unicode/norm"
)

const maxSafeInteger int64 = 1<<53 - 1

var integerPattern = regexp.MustCompile(`^-?(0|[1-9][0-9]*)$`)

// ExecutionBrief is the typed v1 plan passed from a foreman to the execution
// plane. Its nested types deliberately contain no credential-bearing fields.
type ExecutionBrief struct {
	SchemaVersion          string                 `json:"schema_version"`
	BriefID                string                 `json:"brief_id"`
	RevisionID             string                 `json:"revision_id"`
	IssuedAt               string                 `json:"issued_at"`
	IssuedBy               string                 `json:"issued_by"`
	Project                string                 `json:"project"`
	Lane                   string                 `json:"lane"`
	TaskID                 string                 `json:"task_id"`
	TickID                 string                 `json:"tick_id"`
	Mission                string                 `json:"mission"`
	RationaleSummary       string                 `json:"rationale_summary"`
	Source                 Source                 `json:"source"`
	ContextRefs            []ContextRef           `json:"context_refs"`
	Scope                  Scope                  `json:"scope"`
	Constraints            []Constraint           `json:"constraints"`
	NonGoals               []string               `json:"non_goals"`
	CapabilityRequirements CapabilityRequirements `json:"capability_requirements"`
	WorkspacePolicy        WorkspacePolicy        `json:"workspace_policy"`
	PermissionPolicy       PermissionPolicy       `json:"permission_policy"`
	ResourcePolicy         ResourcePolicy         `json:"resource_policy"`
	AcceptanceCriteria     []AcceptanceCriterion  `json:"acceptance_criteria"`
	RequiredDeliverables   []string               `json:"required_deliverables"`
	CloseoutPolicy         CloseoutPolicy         `json:"closeout_policy"`
	Provenance             Provenance             `json:"provenance"`
	BriefDigest            string                 `json:"brief_digest"`
}

// Source identifies the exact repository revisions governed by the brief.
type Source struct {
	RepositoryURI    string `json:"repository_uri"`
	SourceRef        string `json:"source_ref"`
	BaseRevision     string `json:"base_revision"`
	DiffBaseRevision string `json:"diff_base_revision"`
}

// ContextRef records sourced context and its authority.
type ContextRef struct {
	Namespace        string `json:"namespace"`
	KeyOrID          string `json:"key_or_id"`
	RevisionOrDigest string `json:"revision_or_digest"`
	RetrievedAt      string `json:"retrieved_at"`
	Authority        string `json:"authority"`
}

// Scope bounds workspace paths.
type Scope struct {
	AllowedPaths   []string `json:"allowed_paths"`
	ForbiddenPaths []string `json:"forbidden_paths"`
}

// Constraint is an explicit, communicable restriction.
type Constraint struct {
	ID        string `json:"id"`
	Level     string `json:"level"`
	Statement string `json:"statement"`
}

// CapabilityRequirements separates mandatory from advisory connector features.
type CapabilityRequirements struct {
	Required []string `json:"required"`
	Optional []string `json:"optional"`
}

// WorkspacePolicy describes the required workspace containment.
type WorkspacePolicy struct {
	Kind          string   `json:"kind"`
	Isolation     string   `json:"isolation"`
	Network       string   `json:"network"`
	RequiredTools []string `json:"required_tools"`
}

// PermissionPolicy describes unattended permissions in named integer units.
type PermissionPolicy struct {
	Profile          string `json:"profile"`
	Unattended       bool   `json:"unattended"`
	ApprovalTimeoutS int64  `json:"approval_timeout_s"`
}

// ResourcePolicy contains resource limits in named integer units.
type ResourcePolicy struct {
	CPU          int64 `json:"cpu"`
	MemoryMB     int64 `json:"memory_mb"`
	PIDs         int64 `json:"pids"`
	DiskMB       int64 `json:"disk_mb"`
	WallTimeoutS int64 `json:"wall_timeout_s"`
	IdleTimeoutS int64 `json:"idle_timeout_s"`
}

// AcceptanceCriterion is a testable outcome.
type AcceptanceCriterion struct {
	ID                string             `json:"id"`
	Statement         string             `json:"statement"`
	Required          bool               `json:"required"`
	ProofRequirements []ProofRequirement `json:"proof_requirements"`
}

// ProofRequirement is a shell-free deterministic verifier invocation.
type ProofRequirement struct {
	ID               string   `json:"id"`
	Kind             string   `json:"kind"`
	Argv             []string `json:"argv"`
	CWD              string   `json:"cwd"`
	TimeoutS         int64    `json:"timeout_s"`
	ExpectExit       int64    `json:"expect_exit"`
	ExpectedArtifact *string  `json:"expected_artifact"`
}

// CloseoutPolicy states integration requirements.
type CloseoutPolicy struct {
	RequiresCommit           bool   `json:"requires_commit"`
	RequiresSourceOrTestDiff bool   `json:"requires_source_or_test_diff"`
	TargetRemote             string `json:"target_remote"`
	TargetBranch             string `json:"target_branch"`
}

// Provenance ties a brief to its planning and board inputs.
type Provenance struct {
	ForemanRunID  string `json:"foreman_run_id"`
	BoardRevision string `json:"board_revision"`
}

// DecodeBrief decodes one JSON ExecutionBrief and rejects duplicate or unknown
// members, non-NFC collisions, trailing data, and disallowed numbers.
func DecodeBrief(data []byte) (*ExecutionBrief, error) {
	v, err := parseAndNormalize(data)
	if err != nil {
		return nil, err
	}
	normalized, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("marshal normalized brief: %w", err)
	}
	var brief ExecutionBrief
	if err := strictDecode(normalized, &brief); err != nil {
		return nil, fmt.Errorf("decode execution brief: %w", err)
	}
	return &brief, nil
}

// CanonicalBrief returns RFC 8785 JCS bytes after normative NFC normalization,
// number checks, and removal of only the top-level brief_digest member.
func CanonicalBrief(data []byte) ([]byte, error) {
	v, err := parseAndNormalize(data)
	if err != nil {
		return nil, err
	}
	obj, ok := v.(map[string]any)
	if !ok {
		return nil, errors.New("execution brief must be a JSON object")
	}
	delete(obj, "brief_digest")
	intermediate, err := json.Marshal(obj)
	if err != nil {
		return nil, fmt.Errorf("marshal normalized brief: %w", err)
	}
	canonical, err := jsoncanonicalizer.Transform(intermediate)
	if err != nil {
		return nil, fmt.Errorf("JCS canonicalization: %w", err)
	}
	return canonical, nil
}

// BriefDigest returns the sha256-prefixed digest of CanonicalBrief bytes.
func BriefDigest(data []byte) (string, error) {
	canonical, err := CanonicalBrief(data)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(canonical)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// ValidateBriefDigest compares the supplied brief_digest against the digest of
// the normalized canonical bytes. Agent prose is not consulted.
func ValidateBriefDigest(data []byte) error {
	v, err := parseAndNormalize(data)
	if err != nil {
		return err
	}
	obj, ok := v.(map[string]any)
	if !ok {
		return errors.New("execution brief must be a JSON object")
	}
	supplied, ok := obj["brief_digest"].(string)
	if !ok {
		return errors.New("brief_digest must be a string")
	}
	calculated, err := BriefDigest(data)
	if err != nil {
		return err
	}
	if subtle.ConstantTimeCompare([]byte(supplied), []byte(calculated)) != 1 {
		return fmt.Errorf("brief_digest mismatch: calculated %s", calculated)
	}
	return nil
}

func strictDecode(data []byte, dst any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return err
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("trailing JSON value")
	}
	return nil
}

func parseAndNormalize(data []byte) (any, error) {
	if !utf8.Valid(data) {
		return nil, errors.New("input is not valid I-JSON UTF-8")
	}
	if err := rejectUnpairedSurrogates(data); err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	v, err := parseValue(dec, "$")
	if err != nil {
		return nil, err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, errors.New("trailing JSON value")
		}
		return nil, err
	}
	return v, nil
}

// rejectUnpairedSurrogates closes encoding/json's deliberate compatibility
// behavior of replacing malformed UTF-16 escape pairs with U+FFFD. I-JSON
// requires Unicode scalar values, so malformed pairs are rejected instead.
func rejectUnpairedSurrogates(data []byte) error {
	inString := false
	for i := 0; i < len(data); i++ {
		if data[i] == '"' {
			inString = !inString
			continue
		}
		if !inString || data[i] != '\\' {
			continue
		}
		i++
		if i >= len(data) {
			return errors.New("invalid JSON escape")
		}
		if data[i] != 'u' {
			continue
		}
		if i+4 >= len(data) {
			return errors.New("truncated Unicode escape")
		}
		unit, err := strconv.ParseUint(string(data[i+1:i+5]), 16, 16)
		if err != nil {
			continue // the JSON decoder supplies the precise syntax error
		}
		i += 4
		switch {
		case unit >= 0xD800 && unit <= 0xDBFF:
			if i+6 >= len(data) || data[i+1] != '\\' || data[i+2] != 'u' {
				return errors.New("unpaired high surrogate is not I-JSON")
			}
			low, err := strconv.ParseUint(string(data[i+3:i+7]), 16, 16)
			if err != nil || low < 0xDC00 || low > 0xDFFF {
				return errors.New("unpaired high surrogate is not I-JSON")
			}
			i += 6
		case unit >= 0xDC00 && unit <= 0xDFFF:
			return errors.New("unpaired low surrogate is not I-JSON")
		}
	}
	return nil
}

func parseValue(dec *json.Decoder, path string) (any, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	switch t := tok.(type) {
	case json.Delim:
		switch t {
		case '{':
			obj := make(map[string]any)
			for dec.More() {
				keyToken, err := dec.Token()
				if err != nil {
					return nil, err
				}
				rawKey := keyToken.(string)
				key := norm.NFC.String(rawKey)
				if _, exists := obj[key]; exists {
					return nil, fmt.Errorf("%s: duplicate object member %q after NFC normalization", path, key)
				}
				value, err := parseValue(dec, path+"."+key)
				if err != nil {
					return nil, err
				}
				obj[key] = value
			}
			if _, err := dec.Token(); err != nil {
				return nil, err
			}
			return obj, nil
		case '[':
			var values []any
			for i := 0; dec.More(); i++ {
				value, err := parseValue(dec, fmt.Sprintf("%s[%d]", path, i))
				if err != nil {
					return nil, err
				}
				values = append(values, value)
			}
			if _, err := dec.Token(); err != nil {
				return nil, err
			}
			return values, nil
		}
	case string:
		return norm.NFC.String(t), nil
	case json.Number:
		raw := string(t)
		if !integerPattern.MatchString(raw) {
			return nil, fmt.Errorf("%s: non-integer JSON number %q", path, raw)
		}
		n, err := t.Int64()
		if err != nil || n < -maxSafeInteger || n > maxSafeInteger {
			return nil, fmt.Errorf("%s: number %q is outside the I-JSON exact integer range", path, raw)
		}
		return n, nil
	case bool, nil:
		return t, nil
	}
	return nil, fmt.Errorf("%s: unsupported JSON token", path)
}
