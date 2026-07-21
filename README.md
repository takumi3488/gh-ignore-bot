# gh-ignore-bot

GitHub Notifications のうち、Renovate 等の bot が作成した Pull Request のスレッドをまとめて done にする CLI。

通知の一覧取得と done 化は REST API（Notifications は GraphQL 未対応のため）、PR 作者の判定は [gqlgenc](https://github.com/gqlgo/gqlgenc) で生成した GraphQL クライアントで行う。

## Usage

```sh
# 通知 API と GraphQL（公開リポジトリの PR 参照）は notifications スコープのみで動作する
# 非公開リポジトリを含める場合は repo スコープが必要（repo は notifications を兼ねる）
# （通知 API は fine-grained PAT に未対応のため classic PAT を推奨）
export GITHUB_TOKEN=... # GH_TOKEN でも可

gh-ignore-bot           # inbox 内の全 PR 通知（既読含む）のうち bot 作成のものを done にする
gh-ignore-bot --dry-run # 対象の通知を一覧表示するだけ（状態は変更しない）
gh-ignore-bot --unread  # 未読の通知のみを対象にする
gh-ignore-bot --bot 'renovate[bot]' --bot 'my-app[bot]' # 対象 bot を上書き指定
```

bot の判定は `[bot]` サフィックスを除去して比較するため、`renovate` / `renovate[bot]` のどちらの login で返ってきてもマッチする。デフォルトの対象は `renovate[bot]` と `dependabot[bot]`。

## Development

GraphQL クライアントの再生成（要 `GITHUB_TOKEN`、introspection に使用）:

```sh
GITHUB_TOKEN=$(gh auth token) go generate
```
