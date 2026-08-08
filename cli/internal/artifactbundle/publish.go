package artifactbundle

import (
	"fmt"
	"path"
	"strings"
)

func WrapCommand(command []string, runtime Runtime) ([]string, error) {
	if !runtime.Enabled() {
		return command, nil
	}
	if len(command) == 0 {
		return nil, fmt.Errorf("artifact bundle completion requires a Tau-wrappable command")
	}
	script, err := wrapperScript(runtime, `"$@" &`)
	if err != nil {
		return nil, err
	}
	return append([]string{"bash", "-c", script, "tau-bundle-entrypoint"}, command...), nil
}

func WrapShellScript(command string, runtime Runtime) (string, error) {
	if !runtime.Enabled() {
		return command, nil
	}
	if strings.TrimSpace(command) == "" {
		return "", fmt.Errorf("artifact bundle completion requires a non-empty entrypoint")
	}
	return wrapperScript(runtime, "(\n"+command+"\n) &")
}

func wrapperScript(runtime Runtime, launch string) (string, error) {
	manifest, err := runtime.Manifest()
	if err != nil {
		return "", err
	}
	raw, err := Marshal(manifest)
	if err != nil {
		return "", err
	}
	generationManifest := GenerationManifestPath(runtime.OutputDir, runtime.BundleID)
	generationCompletion := GenerationCompletionPath(runtime.OutputDir, runtime.BundleID)
	currentManifest := CurrentManifestPath(runtime.OutputDir)
	currentCompletion := CurrentCompletionPath(runtime.OutputDir)
	publicationCheck := ""
	if manifest.Publication != nil {
		publicationCheck = fmt.Sprintf(`if [ ! -f %s ] || [ "$(cat %s)" != %s ]; then
  echo "artifact bundle refused: staged publication acknowledgement is missing or invalid" >&2
  exit 126
fi
`, shellQuote(manifest.Publication.Completion), shellQuote(manifest.Publication.Completion),
			shellQuote("complete "+manifest.Publication.ID))
	}
	checkpointCheck := ""
	if strings.TrimSpace(runtime.CheckpointIndex) != "" {
		checkpointCheck = fmt.Sprintf(`if [ ! -f %s ] ||
   ! python3 - %s %s <<'TAU_BUNDLE_CHECKPOINT_EOF'
import json, pathlib, sys
index = json.loads(pathlib.Path(sys.argv[1]).read_text())
if index.get("bundle_id") != sys.argv[2]:
    raise SystemExit(1)
TAU_BUNDLE_CHECKPOINT_EOF
then
  echo "artifact bundle refused: declared checkpoint index is missing or belongs to another bundle" >&2
  exit 126
fi
`, shellQuote(runtime.CheckpointIndex), shellQuote(runtime.CheckpointIndex), shellQuote(runtime.BundleID))
	}
	return fmt.Sprintf(`tau_bundle_child=""
tau_bundle_forward_signal() {
  if [ -n "${tau_bundle_child:-}" ]; then
    kill -TERM "$tau_bundle_child" 2>/dev/null || true
  fi
}
trap tau_bundle_forward_signal TERM INT
mkdir -p %s
rm -f %s %s
%s
tau_bundle_child=$!
while :; do
  wait "$tau_bundle_child"
  tau_bundle_status=$?
  if ! kill -0 "$tau_bundle_child" 2>/dev/null; then
    break
  fi
done
trap - TERM INT
if [ "$tau_bundle_status" -ne 0 ]; then
  exit "$tau_bundle_status"
fi
%s%stau_bundle_tmp=%s
if ! printf '%%s' %s > "$tau_bundle_tmp"; then
  rm -f "$tau_bundle_tmp"
  echo "artifact bundle refused: could not write completion manifest" >&2
  exit 126
fi
if [ -e %s ]; then
  if ! cmp -s "$tau_bundle_tmp" %s; then
    rm -f "$tau_bundle_tmp"
    echo "artifact bundle refused: immutable generation manifest already differs" >&2
    exit 126
  fi
  rm -f "$tau_bundle_tmp"
else
  if ! mv -n "$tau_bundle_tmp" %s; then
    rm -f "$tau_bundle_tmp"
    echo "artifact bundle refused: could not commit immutable generation manifest" >&2
    exit 126
  fi
fi
tau_bundle_current_tmp=%s
if ! cp %s "$tau_bundle_current_tmp" ||
   ! mv -f "$tau_bundle_current_tmp" %s; then
  rm -f "$tau_bundle_current_tmp"
  echo "artifact bundle refused: could not publish current manifest" >&2
  exit 126
fi
tau_bundle_marker_tmp=%s
if ! printf 'complete %%s\n' %s > "$tau_bundle_marker_tmp" ||
   ! mv -f "$tau_bundle_marker_tmp" %s; then
  rm -f "$tau_bundle_marker_tmp"
  echo "artifact bundle refused: could not commit generation acknowledgement" >&2
  exit 126
fi
tau_bundle_current_marker_tmp=%s
if ! printf 'complete %%s\n' %s > "$tau_bundle_current_marker_tmp" ||
   ! mv -f "$tau_bundle_current_marker_tmp" %s; then
  rm -f "$tau_bundle_current_marker_tmp"
  echo "artifact bundle refused: could not commit current acknowledgement" >&2
  exit 126
fi
`, shellQuote(path.Dir(generationManifest)), shellQuote(generationCompletion), shellQuote(currentCompletion),
		launch, publicationCheck, checkpointCheck, shellQuote(generationManifest+".tmp.$$"), shellQuote(string(raw)),
		shellQuote(generationManifest), shellQuote(generationManifest), shellQuote(generationManifest),
		shellQuote(currentManifest+".tmp.$$"), shellQuote(generationManifest), shellQuote(currentManifest),
		shellQuote(generationCompletion+".tmp.$$"), shellQuote(runtime.BundleID), shellQuote(generationCompletion),
		shellQuote(currentCompletion+".tmp.$$"), shellQuote(runtime.BundleID), shellQuote(currentCompletion)), nil
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'"'"'`) + "'"
}
