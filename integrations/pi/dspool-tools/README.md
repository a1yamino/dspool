# dspool pi tools

`web_search` and `web_fetch` for [pi](https://github.com/earendil-works/pi), backed by dspool's `/v1/tools/*` proxy.

Both tools use the same key pool, retries, and failover as model requests.

## Automatic install

`dspool pi` writes this extension into `$PI_CODING_AGENT_DIR/extensions/dspool-tools/index.ts` (default `~/.pi/agent/extensions/dspool-tools/index.ts`) before starting pi. pi discovers it automatically.

The extension registers its tools only when `DSPOOL_BASE_URL` or `DSPOOL_API_KEY` is set, so plain pi sessions are unaffected. `dspool pi` exports both variables.

Set `DSPOOL_PI_SEARCH=0` to skip the install.

## Manual install

For a pi session that does not go through `dspool pi`:

```bash
pi install /path/to/dspool/integrations/pi/dspool-tools
export DSPOOL_BASE_URL=http://127.0.0.1:8787
export DSPOOL_API_KEY=dspool-local
```

## Tools

- `web_search` — query DeepSeek's search API. Parameters: `query`, optional `count` (1–50), `need_content`, and `ori_question`.
- `web_fetch` — fetch one HTTP/HTTPS URL as Markdown. Parameter: `url`.

Limit a session to these tools:

```bash
dspool pi --tools web_search,web_fetch
```

Search and fetch requests are billed by DeepSeek separately from model tokens.
