// Package artifactpublish owns Tau's opt-in local-staged artifact publication
// contract. Applications write closed artifacts beneath a pod-local staging
// directory; Tau verifies regular files into a generation-specific durable
// directory and commits a completion marker after every file is durable.
package artifactpublish

import (
	"encoding/base64"
	"fmt"
	"path"
	"strings"
)

const (
	ModeStaged       = "staged"
	CompletionMarker = ".tau-artifacts-complete"
	GenerationsDir   = ".tau-artifacts"
)

type Runtime struct {
	Mode          string
	OutputDir     string
	StagingDir    string
	PublicationID string
}

func (r Runtime) Enabled() bool {
	return strings.TrimSpace(r.Mode) != ""
}

func (r Runtime) Validate() error {
	if r.Mode == "" {
		return nil
	}
	if r.Mode != ModeStaged {
		return fmt.Errorf("storage.publish must be %q or empty", ModeStaged)
	}
	output := path.Clean(strings.TrimSpace(r.OutputDir))
	if output == "." || (output != "/data" && !strings.HasPrefix(output, "/data/")) {
		return fmt.Errorf("staged artifact publication requires storage.output under /data")
	}
	staging := path.Clean(strings.TrimSpace(r.StagingDir))
	if staging == "." || (staging != "/mnt" && !strings.HasPrefix(staging, "/mnt/")) {
		return fmt.Errorf("staged artifact publication requires a local staging directory under /mnt")
	}
	publicationID := strings.TrimSpace(r.PublicationID)
	if publicationID == "" {
		return fmt.Errorf("staged artifact publication requires a publication ID")
	}
	if publicationID == "." || publicationID == ".." || strings.ContainsAny(publicationID, `/\`) {
		return fmt.Errorf("staged artifact publication ID must be a single path segment")
	}
	return nil
}

func (r Runtime) Env() map[string]string {
	if !r.Enabled() {
		return nil
	}
	return map[string]string{
		"TAU_OUTPUT_DIR":         path.Clean(r.OutputDir),
		"TAU_OUTPUT_STAGING_DIR": path.Clean(r.StagingDir),
	}
}

func (r Runtime) PublishedDir() string {
	return path.Join(path.Clean(r.OutputDir), GenerationsDir, strings.TrimSpace(r.PublicationID))
}

func WrapCommand(command []string, runtime Runtime) ([]string, error) {
	if len(command) == 0 {
		return nil, fmt.Errorf("staged artifact publication requires a Tau-wrappable script or explicit command")
	}
	script, err := wrapperScript(runtime, `"$@" &`)
	if err != nil {
		return nil, err
	}
	return append([]string{"bash", "-c", script, "tau-artifact-entrypoint"}, command...), nil
}

func WrapShellScript(command string, runtime Runtime) (string, error) {
	if strings.TrimSpace(command) == "" {
		return "", fmt.Errorf("staged artifact publication requires a non-empty entrypoint")
	}
	encoded := base64.StdEncoding.EncodeToString([]byte(command))
	launch := fmt.Sprintf(`tau_publish_script=/tmp/tau-artifact-entrypoint.$$.sh
printf '%%s' %s | base64 -d > "$tau_publish_script"
chmod 0700 "$tau_publish_script"
"$tau_publish_script" &`, shellQuote(encoded))
	return wrapperScript(runtime, launch)
}

func wrapperScript(runtime Runtime, launch string) (string, error) {
	if err := runtime.Validate(); err != nil {
		return "", err
	}
	return fmt.Sprintf(`tau_publish_output=%s
tau_publish_staging=%s
tau_publish_id=%s
tau_publish_generation=%s
tau_publish_child=""
tau_publish_script=""
tau_publish_cleanup() {
  if [ -n "$tau_publish_script" ]; then
    rm -f "$tau_publish_script"
  fi
}
tau_publish_forward_signal() {
  if [ -n "${tau_publish_child:-}" ]; then
    kill -TERM "$tau_publish_child" 2>/dev/null || true
  fi
}
trap tau_publish_cleanup EXIT
trap tau_publish_forward_signal TERM INT
mkdir -p "$tau_publish_staging"
mkdir -p "$tau_publish_generation"
rm -f "$tau_publish_generation/%s"
export TAU_OUTPUT_DIR="$tau_publish_output"
export TAU_OUTPUT_STAGING_DIR="$tau_publish_staging"
%s
tau_publish_child=$!
while :; do
  wait "$tau_publish_child"
  tau_publish_status=$?
  if ! kill -0 "$tau_publish_child" 2>/dev/null; then
    break
  fi
done
trap - TERM INT
if [ "$tau_publish_status" -ne 0 ]; then
  exit "$tau_publish_status"
fi
tau_publish_sha256() {
  tau_publish_digest_line="$(sha256sum "$1" 2>/dev/null)" || return 1
  tau_publish_digest="${tau_publish_digest_line%% *}"
  [ -n "$tau_publish_digest" ] || return 1
  printf '%%s\n' "$tau_publish_digest"
}
for tau_publish_source in "$tau_publish_staging"/* "$tau_publish_staging"/.[!.]* "$tau_publish_staging"/..?*; do
  [ -e "$tau_publish_source" ] || [ -L "$tau_publish_source" ] || continue
  tau_publish_name="$(basename "$tau_publish_source")"
  tau_publish_destination="$tau_publish_generation/$tau_publish_name"
  if [ "$tau_publish_name" = "%s" ] || [ "$tau_publish_name" = "%s" ]; then
    echo "staged artifact name $tau_publish_name is reserved by Tau" >&2
    exit 126
  fi
  if [ -L "$tau_publish_source" ] || [ ! -f "$tau_publish_source" ]; then
    echo "staged artifact $tau_publish_source is not a regular file; publish directories with an application-owned durable protocol" >&2
    exit 126
  fi
  if ! tau_publish_source_digest="$(tau_publish_sha256 "$tau_publish_source")"; then
    echo "failed to checksum staged artifact $tau_publish_name" >&2
    exit 126
  fi
  if [ -e "$tau_publish_destination" ]; then
    tau_publish_destination_digest=""
    if [ ! -L "$tau_publish_destination" ] &&
       [ -f "$tau_publish_destination" ] &&
       tau_publish_destination_digest="$(tau_publish_sha256 "$tau_publish_destination")" &&
       [ "$tau_publish_destination_digest" = "$tau_publish_source_digest" ]; then
      continue
    fi
    echo "refusing to overwrite non-matching durable artifact $tau_publish_destination" >&2
    exit 126
  fi
  tau_publish_tmp="$tau_publish_generation/.tau-publish-${tau_publish_name}.$$"
  rm -f "$tau_publish_tmp"
  if ! cp -a "$tau_publish_source" "$tau_publish_tmp"; then
    rm -f "$tau_publish_tmp"
    echo "failed to copy staged artifact $tau_publish_name" >&2
    exit 126
  fi
  tau_publish_tmp_digest=""
  if ! tau_publish_tmp_digest="$(tau_publish_sha256 "$tau_publish_tmp")" ||
     [ "$tau_publish_tmp_digest" != "$tau_publish_source_digest" ]; then
    rm -f "$tau_publish_tmp"
    echo "checksum mismatch while staging durable artifact $tau_publish_name" >&2
    exit 126
  fi
  if ! mv -n "$tau_publish_tmp" "$tau_publish_destination"; then
    rm -f "$tau_publish_tmp"
    echo "failed to publish staged artifact $tau_publish_name" >&2
    exit 126
  fi
  if [ -e "$tau_publish_tmp" ]; then
    tau_publish_destination_digest=""
    if tau_publish_destination_digest="$(tau_publish_sha256 "$tau_publish_destination")" &&
       [ "$tau_publish_destination_digest" = "$tau_publish_source_digest" ]; then
      rm -f "$tau_publish_tmp"
      continue
    fi
    rm -f "$tau_publish_tmp"
    echo "concurrent publication committed a non-matching durable artifact $tau_publish_destination" >&2
    exit 126
  fi
  tau_publish_destination_digest=""
  if ! tau_publish_destination_digest="$(tau_publish_sha256 "$tau_publish_destination")" ||
     [ "$tau_publish_destination_digest" != "$tau_publish_source_digest" ]; then
    echo "checksum mismatch after publishing durable artifact $tau_publish_name" >&2
    exit 126
  fi
done
tau_publish_marker_tmp="$tau_publish_generation/%s.tmp.$$"
if ! printf 'complete %%s\n' "$tau_publish_id" > "$tau_publish_marker_tmp"; then
  rm -f "$tau_publish_marker_tmp"
  echo "failed to write artifact publication marker" >&2
  exit 126
fi
if ! mv "$tau_publish_marker_tmp" "$tau_publish_generation/%s"; then
  rm -f "$tau_publish_marker_tmp"
  echo "failed to commit artifact publication marker" >&2
  exit 126
fi
`, shellQuote(path.Clean(runtime.OutputDir)), shellQuote(path.Clean(runtime.StagingDir)),
		shellQuote(strings.TrimSpace(runtime.PublicationID)), shellQuote(runtime.PublishedDir()),
		CompletionMarker, launch, CompletionMarker, GenerationsDir, CompletionMarker, CompletionMarker), nil
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'"'"'`) + "'"
}
