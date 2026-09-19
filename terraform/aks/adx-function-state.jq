# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

def nonempty_string: type == "string" and length > 0;
def dns_label: type == "string" and length <= 63 and test("^[a-z0-9]([-a-z0-9]*[a-z0-9])?$");
def dns_name: type == "string" and length <= 253 and test("^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$");
def positive_integer: type == "number" and . > 0 and floor == .;
def identity: "\(.metadata.namespace)/\(.metadata.name)";
def owned:
  .metadata.labels["app.kubernetes.io/managed-by"] == "Helm" and
  .metadata.labels["app.kubernetes.io/instance"] == $release and
  .metadata.annotations["meta.helm.sh/release-name"] == $release and
  .metadata.annotations["meta.helm.sh/release-namespace"] == $releaseNamespace;

if length != 1 then error("Expected exactly one complete JSON source.") else .[0] end |
if ($release | dns_name | not) or ($release | length) > 53 or ($releaseNamespace | dns_label | not) then
  error("Invalid Helm release name or release namespace.")
elif $mode == "expected" then
  if type != "array" or length == 0 then error("Empty or malformed rendered manifest.") else . end |
  map(select(. != null)) |
  if length == 0 or any(type != "object" or (.apiVersion | nonempty_string | not) or (.kind | nonempty_string | not)) then
    error("Empty or malformed rendered manifest.")
  elif any(.kind == "List" or (.kind == "Function" and .apiVersion != "adx-mon.azure.com/v1")) then
    error("Unsupported rendered Function API or ambiguous List manifest.")
  else . end |
  [.[] | select(.kind == "Function") |
    if (.metadata.name | dns_name | not) or (.metadata.namespace | dns_label | not) or
       .metadata.labels["app.kubernetes.io/managed-by"] != "Helm" or
       .metadata.labels["app.kubernetes.io/instance"] != $release then
      error("Cannot establish intended Function identity/release from rendered manifest.")
    else {name: .metadata.name, namespace: .metadata.namespace} end] |
  if (unique | length) != length then error("Duplicate rendered Function identity.") else . end
elif $mode == "preflight" or $mode == "state" then
  if type != "object" or (.kind != "FunctionList" and .kind != "List") or
     (.apiVersion != "adx-mon.azure.com/v1" and (.kind != "List" or .apiVersion != "v1")) or
     (.items | type) != "array" or ((.metadata.continue // "") != "") then
    error("Incomplete or malformed Function list for namespace \($namespace).")
  else .items end |
  if any(type != "object" or .apiVersion != "adx-mon.azure.com/v1" or .kind != "Function" or
         (.metadata.name | dns_name | not) or .metadata.namespace != $namespace) then
    error("Malformed Function identity in namespace \($namespace).")
  elif (map(.metadata.name) | unique | length) != length then
    error("Duplicate Function identity in namespace \($namespace).")
  else . end |
  INDEX(.metadata.name) as $objects |
  [$expected[] | select(.namespace == $namespace) |
    . as $required | $objects[.name] as $object |
    if $object == null then
      $required + {phase: "Pending", diagnostic: "\(.namespace)/\(.name): missing"}
    else
      $object |
      if (owned | not) then
        error("Function ownership conflict at \(identity); expected Helm release \($release) in \($releaseNamespace). No adoption or deletion is permitted.")
      elif (.metadata.uid | nonempty_string | not) or (.metadata.resourceVersion | nonempty_string | not) or
           (.metadata.generation | positive_integer | not) then
        error("Missing or malformed Function UID/resourceVersion/generation at \(identity).")
      else . end |
      {name: .metadata.name, namespace: .metadata.namespace, uid: .metadata.uid,
       resourceVersion: .metadata.resourceVersion, generation: .metadata.generation,
       observedGeneration: .status.observedGeneration, status: .status.status, error: .status.error,
       deleting: (.metadata.deletionTimestamp != null)} |
      if $mode == "preflight" then . + {phase: "Pending"}
      elif (.status != null and (.status | type) != "string") or
           (.error != null and (.error | type) != "string") or
           (.observedGeneration != null and (.observedGeneration | positive_integer | not)) then
        error("Malformed Function status at \(.namespace)/\(.name).")
      else
        . + {phase: (
          if .deleting or .generation != .observedGeneration then "Pending"
          elif .status == "Success" then "Success"
          elif .status == "PermanentFailure" then
            if ((.error // "") | test("throttl|requestratelimitpolicy|toomanyrequests"; "i")) then "Throttle" else "Terminal" end
          else "Pending" end)}
      end |
      . + {diagnostic: "\(.namespace)/\(.name) (generation=\(.generation), observedGeneration=\(.observedGeneration), status=\(.status), error=\(.error))"}
    end]
else error("Unknown Function validation mode.") end
