package envspec

import (
	"strings"
	"testing"
)

func TestRenderYAMLQuotesEnvNames(t *testing.T) {
	rendered := RenderYAML([]Var{
		direct("ON", "1"),
		Secret("NO", "hf-token", "token"),
	}, 2)

	for _, want := range []string{
		`  - name: "ON"`,
		`    value: "1"`,
		`  - name: "NO"`,
		`    valueFrom:`,
		`      secretKeyRef:`,
		`        name: "hf-token"`,
		`        key: "token"`,
	} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("rendered YAML missing %q:\n%s", want, rendered)
		}
	}
}

func TestRedactSecretRefsKeepsOriginalInput(t *testing.T) {
	original := []Var{Secret("HF_TOKEN", "hf-secret", "token-key")}
	redacted := RedactSecretRefs(original)

	if got := original[0].ValueFrom.SecretKeyRef.Name; got != "hf-secret" {
		t.Fatalf("original secret ref was mutated: %q", got)
	}
	ref := redacted[0].ValueFrom.SecretKeyRef
	if ref.Name != "<redacted>" || ref.Key != "<redacted>" {
		t.Fatalf("redacted ref = %+v", ref)
	}
}

func TestParseSecretKeyRefSpec(t *testing.T) {
	ref, err := ParseSecretKeyRefSpec("hf-secret:token-key")
	if err != nil {
		t.Fatal(err)
	}
	if ref.Name != "hf-secret" || ref.Key != "token-key" {
		t.Fatalf("ref = %+v", ref)
	}
	for _, spec := range []string{"hf-secret", ":token-key", "hf-secret:", " hf-secret:token-key", "hf-secret: token-key"} {
		if _, err := ParseSecretKeyRefSpec(spec); err == nil {
			t.Fatalf("expected %q to fail", spec)
		}
	}
}
