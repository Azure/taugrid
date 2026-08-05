package expstore

// workspaceRunClause scopes a run search to one workspace.
//
// Untagged runs are admitted: Stellar serves exactly one workspace, and runs
// written before tau_workspace was stamped carry no tag. Filtering them out
// would silently empty every dashboard rather than deny access to anything.
//
// Placeholder order: tag key, workspace, tag key.
const workspaceRunClause = `(EXISTS (
  SELECT 1 FROM tags wt WHERE wt.scope_type = 'run' AND wt.scope_id = r.run_id AND wt.key = ? AND wt.value = ?
) OR NOT EXISTS (
  SELECT 1 FROM tags wt WHERE wt.scope_type = 'run' AND wt.scope_id = r.run_id AND wt.key = ?
))`
