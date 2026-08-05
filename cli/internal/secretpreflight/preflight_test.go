package secretpreflight

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/Azure/taugrid/core/envspec"
)

type fakeRunner struct {
	calls     [][]string
	responses []fakeResponse
}

type fakeResponse struct {
	out string
	err error
}

func (f *fakeRunner) Raw(_ context.Context, args []string, _ []byte) (string, error) {
	f.calls = append(f.calls, append([]string(nil), args...))
	if len(f.responses) == 0 {
		return "", errors.New("unexpected kubectl call")
	}
	response := f.responses[0]
	f.responses = f.responses[1:]
	return response.out, response.err
}

func TestValidateRequiredEnvAcceptsExistingSecretKeysWithoutReadingValues(t *testing.T) {
	runner := &fakeRunner{responses: []fakeResponse{
		{out: "yes\n"},
		{out: "key\nurl\n"},
	}}
	vars := []envspec.Var{
		envspec.Secret("CDS_URL", "aurora-solar-cds-credentials", "url"),
		envspec.Secret("CDS_KEY", "aurora-solar-cds-credentials", "key"),
	}

	if err := ValidateRequiredEnv(context.Background(), runner, "tau-default", vars); err != nil {
		t.Fatal(err)
	}
	wantGet := []string{
		"get", "secret", "-n", "tau-default", "-o", secretKeysTemplate, "--", "aurora-solar-cds-credentials",
	}
	if !reflect.DeepEqual(runner.calls[1], wantGet) {
		t.Fatalf("get call = %#v, want %#v", runner.calls[1], wantGet)
	}
}

func TestValidateRequiredEnvRejectsInvalidSecretNameBeforeKubectl(t *testing.T) {
	runner := &fakeRunner{}
	err := ValidateRequiredEnv(context.Background(), runner, "tau-default", []envspec.Var{
		envspec.Secret("TOKEN", "--server=http://attacker.invalid", "token"),
	})
	if err == nil || !strings.Contains(err.Error(), `required Secret name "--server=http://attacker.invalid" is invalid`) {
		t.Fatalf("error = %v", err)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("kubectl calls = %#v, want none for invalid Secret name", runner.calls)
	}
}

func TestValidateRequiredEnvRejectsMissingSecret(t *testing.T) {
	runner := &fakeRunner{responses: []fakeResponse{
		{out: "yes\n"},
		{err: errors.New(`Error from server (NotFound): secrets "missing" not found`)},
	}}

	err := ValidateRequiredEnv(context.Background(), runner, "tau-default", []envspec.Var{
		envspec.Secret("TOKEN", "missing", "token"),
	})
	if err == nil || !strings.Contains(err.Error(), "required Secret tau-default/missing does not exist") {
		t.Fatalf("error = %v", err)
	}
}

func TestValidateRequiredEnvRejectsMissingKey(t *testing.T) {
	runner := &fakeRunner{responses: []fakeResponse{
		{out: "yes\n"},
		{out: "url\n"},
	}}

	err := ValidateRequiredEnv(context.Background(), runner, "tau-default", []envspec.Var{
		envspec.Secret("CDS_URL", "credentials", "url"),
		envspec.Secret("CDS_KEY", "credentials", "key"),
	})
	if err == nil || err.Error() != "required Secret tau-default/credentials is missing keys: key" {
		t.Fatalf("error = %v", err)
	}
}

func TestValidateRequiredEnvRejectsPermissionDeniedWithoutGettingSecret(t *testing.T) {
	runner := &fakeRunner{responses: []fakeResponse{{out: "no\n", err: errors.New("exit status 1")}}}

	err := ValidateRequiredEnv(context.Background(), runner, "tau-default", []envspec.Var{
		envspec.Secret("TOKEN", "restricted", "token"),
	})
	if err == nil || !strings.Contains(err.Error(), "current identity cannot get this Secret") {
		t.Fatalf("error = %v", err)
	}
	if len(runner.calls) != 1 {
		t.Fatalf("kubectl calls = %#v, want auth check only", runner.calls)
	}
}

func TestValidateRequiredEnvUsesPendingSecretWithoutClusterRead(t *testing.T) {
	runner := &fakeRunner{}
	err := ValidateRequiredEnv(context.Background(), runner, "tau-default", []envspec.Var{
		envspec.Secret("TOKEN", "job-secret", "token"),
	}, AvailableSecret{Name: "job-secret", Keys: []string{"token"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("kubectl calls = %#v, want none for pending Secret", runner.calls)
	}
}

func TestValidateRequiredEnvRejectsMissingPendingSecretKeyWithoutClusterRead(t *testing.T) {
	runner := &fakeRunner{}
	err := ValidateRequiredEnv(context.Background(), runner, "tau-default", []envspec.Var{
		envspec.Secret("TOKEN", "job-secret", "token"),
	}, AvailableSecret{Name: "job-secret", Keys: []string{"other"}})
	if err == nil || err.Error() != "required Secret tau-default/job-secret is missing keys: token" {
		t.Fatalf("error = %v", err)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("kubectl calls = %#v, want none for pending Secret", runner.calls)
	}
}

func TestValidateRequiredEnvIgnoresLiteralEnv(t *testing.T) {
	runner := &fakeRunner{}
	if err := ValidateRequiredEnv(context.Background(), runner, "tau-default", envspec.FromMap(map[string]string{"MODE": "test"})); err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("kubectl calls = %#v, want none", runner.calls)
	}
}
