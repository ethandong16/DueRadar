# DueRadar contributor instructions

Use REQUIREMENTS.md as the product contract. Never commit credentials, personal task snapshots, `.local`, `data`, or `artifacts`. Run `go test ./...` and `go vet ./...` before a milestone commit.

For the active collaborative development session, if a task message is unreadable, consult the matching plaintext handoff in `.local`: rain reads `rain-task.md`; device reads `device-next.md`; core reads `core-review.md`; web reads `root-contract.md`; zhidao_login reads `zhidao-login-task.md`. These local handoffs define file ownership; do not overlap edits.
