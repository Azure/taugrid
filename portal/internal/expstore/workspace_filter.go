// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package expstore

const workspaceRunClause = `(EXISTS (
  SELECT 1 FROM tags wt WHERE wt.scope_type = 'run' AND wt.scope_id = r.run_id AND wt.key = ? AND wt.value = ?
) OR NOT EXISTS (
  SELECT 1 FROM tags wt WHERE wt.scope_type = 'run' AND wt.scope_id = r.run_id AND wt.key = ?
))`
