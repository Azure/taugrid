# Pull request reviewers

`CODEOWNERS` requests the existing `Azure/taugrid-maintainers` team for repository changes.
The additional-reviewer workflow requests `AaronLiChen` as an individual reviewer.
This supports individual-review-request discovery without granting repository write access or changing code ownership.

The workflow acts when a pull request opens, reopens, or becomes ready for review.
It skips drafts, closed pull requests, self-authored pull requests, and requests that already exist.
It preserves other individual and team requests.
Existing open pull requests do not receive a retroactive request solely because this workflow merges.

Only the upstream `Azure/taugrid` repository sends these requests.
The privileged job uses the GitHub API and does not check out or execute pull request code.
A separate read-only pull-request job runs the synthetic tests.

GitHub can clear an individual request after a submitted review.
This workflow does not request another review on every push.
Review controllers must retain enrolled ownership through later commits and necessary reapproval until actual merge or close.

Run the local tests with:

```bash
node --test .github/tests/request-additional-reviewer.test.mjs
```
