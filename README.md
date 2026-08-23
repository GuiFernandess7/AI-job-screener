# AI Job Screener

Go monorepo with two services isolated as separate modules:

- `services/function`: HTTP function responsible for the initial capture of job postings.
- `services/worker`: batch worker responsible for screening and notification.

## Demo

https://github.com/user-attachments/assets/88f62a04-6791-4d96-a33c-87dec536fbbb

The repository uses Go Workspaces (`go.work`, Go 1.25) for joint development, but each service keeps its own `go.mod`, dependencies and deploy pipeline.

## Structure

```text
services/
  function/
    cmd/local/
    internal/http/
    internal/jobs/
    function.go
    go.mod
  worker/
    cmd/worker/
    internal/jobs/
    Dockerfile
    worker.go
    go.mod
deploy/
  function.cloudbuild.yaml
  worker.cloudbuild.yaml
go.work
```

## Local environment

Create a `.env` file at the repository root:

```dotenv
DATABASE_URL=postgresql://user:password@host/database?sslmode=require
LLM_API_URL=https://api.siliconflow.com/v1/chat/completions
LLM_API_KEY=your-llm-api-key
LLM_MODEL=deepseek-ai/DeepSeek-V3
DISCORD_WEBHOOK_URL=https://discord.com/api/webhooks/your/webhook
```

Both services try to load `.env` from the current directory and from the monorepo root.

## Development

To build both modules in the workspace:

```bash
go build ./services/function/...
go build ./services/worker/...
```

### Function locally

In PowerShell:

```powershell
$env:FUNCTION_TARGET="Trigger"
$env:LOCAL_ONLY="true"
go run ./services/function/cmd/local
```

The function receives `POST http://localhost:8080/` and forwards internally to `/trigger`.

Optional payload:

```json
{
  "terms": ["golang", "python backend"]
}
```

### Worker locally

In PowerShell:

```powershell
go run ./services/worker/cmd/worker
```

The worker fetches every job with status `PENDENTE`, processes each item with jitter between requests, calls the configured LLM for a structured review, and notifies Discord with the outcome.

The LLM call follows the `POST /v1/chat/completions` format, with `Authorization: Bearer`, `Content-Type: application/json`, a `model` field and a single `messages` item with `role: user`. The response is read from `choices[0].message.content`, equivalent to `resp.raise_for_status(); resp.json()["choices"][0]["message"]["content"]`.

## Job review and notification

The LLM returns a structured JSON review for each job, containing the fields `decisao` (`APROVADO` or `REJEITADO`), `titulo_vaga`, `empresa`, `status_processamento` and `justificativa`.

Every processed job — approved or rejected — is posted to the Discord webhook as an embed, color-coded by decision:

- **Green** (`3066993`) for `APROVADO`.
- **Red** (`15158332`) for `REJEITADO`.

The embed includes the job title, company, job ID, decision and application link. Jobs that fail LLM review, LinkedIn detail fetching, or the Discord webhook call are marked as `ERRO` in the database instead of `PROCESSADO`.

## Service responsibilities

### Function

- HTTP stack with Echo and middlewares.
- Endpoint `POST /trigger`.
- LinkedIn Guest API scraper.
- Initial job persistence in PostgreSQL with deduplication.

### Worker

- Batch read of pending jobs.
- Enrichment via LinkedIn's public job detail page.
- Integration with an LLM for structured decision-making.
- Discord webhook notification for every processed job.
- Status update to `PROCESSADO` or `ERRO`.

## Isolated deploys

### Function

```bash
gcloud builds submit --config deploy/function.cloudbuild.yaml .
```

This pipeline deploys the Cloud Function Gen 2 (runtime `go125`) using only `services/function` as the service source.

Before the first deploy, create a Secret Manager secret with the PostgreSQL connection string:

```bash
printf '%s' 'postgresql://user:password@host/database?sslmode=require' | \
gcloud secrets create DATABASE_URL --data-file=-
```

If the secret already exists and you want to update its value:

```bash
printf '%s' 'postgresql://user:password@host/database?sslmode=require' | \
gcloud secrets versions add DATABASE_URL --data-file=-
```

The secret name can be changed at deploy time via the `_DATABASE_URL_SECRET` substitution.

### Worker

```bash
gcloud builds submit --config deploy/worker.cloudbuild.yaml .
```

This pipeline builds the worker image from `services/worker` and publishes it as a separate Cloud Run Job.

Before the first worker deploy, create the required Secret Manager secrets:

```bash
printf '%s' 'postgresql://user:password@host/database?sslmode=require' | \
gcloud secrets create DATABASE_URL --data-file=-

printf '%s' 'your-llm-api-key' | \
gcloud secrets create LLM_API_KEY --data-file=-

printf '%s' 'https://discord.com/api/webhooks/...' | \
gcloud secrets create DISCORD_WEBHOOK_URL --data-file=-
```

If the secrets already exist and you want to update their values:

```bash
printf '%s' 'postgresql://user:password@host/database?sslmode=require' | \
gcloud secrets versions add DATABASE_URL --data-file=-

printf '%s' 'your-llm-api-key' | \
gcloud secrets versions add LLM_API_KEY --data-file=-

printf '%s' 'https://discord.com/api/webhooks/...' | \
gcloud secrets versions add DISCORD_WEBHOOK_URL --data-file=-
```

Secret names can be changed via `_DATABASE_URL_SECRET`, `_LLM_API_KEY_SECRET` and `_DISCORD_WEBHOOK_SECRET`.
