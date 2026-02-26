# Telegram Update Dumper Bot

Replies to every (private) message with the update JSON from the Telegram Bot API. Sends as a document so that it can support a wider range of updates without being constrained by the 4096 characters limit.

This is the code that powers [@dumptgjsonbot](https://t.me/dumptgjsonbot).

100% coded by GLM-4.6 and Codex. Improved by Claude 4.6 Sonnet.

## Running

1. Set `TELEGRAM_BOT_TOKEN` to your bot token.
2. Run with `go run .`.
