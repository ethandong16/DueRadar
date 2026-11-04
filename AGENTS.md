# DueRadar contributor instructions

Use REQUIREMENTS.md as the product contract. Never commit credentials, personal task snapshots, `.local`, `data`, or `artifacts`. Run `go test ./...` and `go vet ./...` before a milestone commit.

For the active collaborative development session, if a task message is unreadable, consult the matching plaintext handoff in `.local`: rain reads `rain-task.md`; device reads `device-next.md`; core reads `core-review.md`; web reads `root-contract.md`; zhidao_login reads `zhidao-login-task.md`. These local handoffs define file ownership; do not overlap edits.

## Handing interactive steps to the user's phone

When a step needs the user to act on their phone (Zhidao QR binding, login verification, SMS confirmation), post the QR code or screenshot directly into the conversation as Markdown with an absolute forward-slash path, for example `![知到登录二维码](C:/Users/<user>/AppData/Local/Temp/zhidao-test/qr.png)`. Writing the image to a file and only printing its path is not enough — the user cannot act on it from a path.

Keep these temporary files outside the repository (under the OS temp directory), and delete them together with any credentials or session tokens when the test ends.

