package execution

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

type vectorFile struct {
	Positive []struct{ Name, Input, EquivalentInput, AbsentInput, Canonical, Digest string } `json:"positive"`
	Negative []struct{ Name, Operation, Input, ErrorContains string }                        `json:"negative"`
}

func loadVectors(t *testing.T) vectorFile {
	t.Helper()
	b, err := os.ReadFile("testdata/canonicalization-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	var vectors vectorFile
	if err := json.Unmarshal(b, &vectors); err != nil {
		t.Fatal(err)
	}
	return vectors
}

func TestCanonicalizationVectors(t *testing.T) {
	for _, vector := range loadVectors(t).Positive {
		t.Run(vector.Name, func(t *testing.T) {
			got, err := CanonicalBrief([]byte(vector.Input))
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != vector.Canonical {
				t.Fatalf("canonical = %q, want %q", got, vector.Canonical)
			}
			digest, err := BriefDigest([]byte(vector.Input))
			if err != nil {
				t.Fatal(err)
			}
			if digest != vector.Digest {
				t.Fatalf("digest = %q, want %q", digest, vector.Digest)
			}
			if vector.EquivalentInput != "" {
				equivalent, err := BriefDigest([]byte(vector.EquivalentInput))
				if err != nil {
					t.Fatal(err)
				}
				if equivalent != digest {
					t.Fatalf("NFC-equivalent digest = %q, want %q", equivalent, digest)
				}
			}
			if vector.AbsentInput != "" {
				absent, err := BriefDigest([]byte(vector.AbsentInput))
				if err != nil {
					t.Fatal(err)
				}
				if absent == digest {
					t.Fatal("absent and explicit null must have different digests")
				}
			}
		})
	}
}

func TestCanonicalizationNegativeVectors(t *testing.T) {
	for _, vector := range loadVectors(t).Negative {
		t.Run(vector.Name, func(t *testing.T) {
			var err error
			switch vector.Operation {
			case "decode":
				_, err = DecodeBrief([]byte(vector.Input))
			case "validate_digest":
				err = ValidateBriefDigest([]byte(vector.Input))
			default:
				_, err = CanonicalBrief([]byte(vector.Input))
			}
			if err == nil || !strings.Contains(err.Error(), vector.ErrorContains) {
				t.Fatalf("error = %v, want containing %q", err, vector.ErrorContains)
			}
		})
	}
}

func TestDecodeBriefRejectsUnknownMembers(t *testing.T) {
	_, err := DecodeBrief([]byte(`{"schema_version":"1.0","credential":"secret"}`))
	if err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("error = %v, want unknown field", err)
	}
	_, err = DecodeBrief([]byte(`{"source":{"repository_uri":"https://git.example/project.git","source_ref":"main","base_revision":"abc","diff_base_revision":"abc","private_key":"not-allowed"}}`))
	if err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("nested error = %v, want unknown field", err)
	}
}

func TestValidateBriefDigest(t *testing.T) {
	input := []byte(`{"brief_digest":"sha256:6a86b48700e811fafafd4b305b0fba0af42d68c3278954f685156cad7b509a45","text":"quote\" slash/ back\\ control\u0001"}`)
	if err := ValidateBriefDigest(input); err != nil {
		t.Fatal(err)
	}
	bad := []byte(`{"brief_digest":"sha256:0000000000000000000000000000000000000000000000000000000000000000","text":"agent says digest is valid"}`)
	if err := ValidateBriefDigest(bad); err == nil || !strings.Contains(err.Error(), "mismatch") {
		t.Fatalf("error = %v, want mismatch", err)
	}
}
