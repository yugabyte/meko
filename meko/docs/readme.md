# Meko documentation

Hugo source for [docs.mekodata.ai](https://docs.mekodata.ai/).

## Preview locally

You need Docker. Colima on macOS is fine.

The site comes up without Kapa credentials. Ask Meko (the in-page chat) does not: the proxy returns HTTP 500 with `Missing KAPA_API_KEY or KAPA_PROJECT_ID` until you set them.

Run these from the `meko/` directory (the parent of this `docs/` folder).

### 1. Kapa credentials (only if you want chat)

Copy the sample env file and fill it in from [app.kapa.ai](https://app.kapa.ai) (API Keys, Projects, Integrations):

```bash
cp docs/docker/.env.sample docs/docker/.env
```

```bash
KAPA_API_KEY=
KAPA_PROJECT_ID=
KAPA_INTEGRATION_ID=
KAPA_SOURCE_GROUP_IDS=
```

`docs/docker/.env` is gitignored. `docker compose` reads it from `docs/docker/`.

| Variable | Required | If unset |
|---|---|---|
| `KAPA_API_KEY` | yes | chat 500 |
| `KAPA_PROJECT_ID` | yes | chat 500 |
| `KAPA_INTEGRATION_ID` | no | chat works; Kapa analytics stay unscoped |
| `KAPA_SOURCE_GROUP_IDS` | no | answers use every source in the project |

`KAPA_INTEGRATION_ID` must be a Custom (via API) integration id. Widget-type ids are rejected by Kapa's HTTP API. `KAPA_SOURCE_GROUP_IDS` is a comma-separated list.

### 2. Start the stack

```bash
./docs-dev.sh start
```

That builds the image and starts two containers:

- docs site: [http://localhost:13131/](http://localhost:13131/)
- Kapa proxy: `http://localhost:8787/api/kapa/chat`

Hugo in development points Ask Meko at the proxy via `config/development/params.toml`. Production uses `/api/kapa/chat` on the same origin (a Netlify Function).

Stop with `./docs-dev.sh stop`. Other commands: `logs`, `shell`, `build`, `clean`, `setup-fork`.

## Production

Set the same four variables on the docs Netlify site. The function declares `path: /api/kapa/chat`, so you do not add a redirect. Chat 500s on the first deploy that includes this code if the key and project id are missing.
