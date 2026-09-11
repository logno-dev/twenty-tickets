# Twenty Tickets

One Go service receives Resend email webhooks and creates records in one or more Twenty instances. A built-in admin UI manages connections, inbound recipient routes, and field mappings using Twenty's object metadata. Docker/Coolify deployment needs no separate frontend or database container.

## Quick start in Coolify

1. Deploy this repository with the **Dockerfile** build pack.
2. Set the exposed/container port and `PORT` to **8080**.
3. Assign an HTTPS domain, for example `https://tickets.example.com`. Coolify handles TLS and routes to the container over HTTP.
4. Mount persistent storage at **`/data`**. The container runs as UID/GID **10001:10001**, which must be able to write to the mount. A fresh named Docker volume inherits the image directory's ownership; existing bind mounts may need their ownership adjusted.
5. Add these **runtime** environment variables (disable Build Variable for credentials):

   ```env
   RESEND_API_KEY=re_your_key
   RESEND_WEBHOOK_SECRET=whsec_your_signing_secret
   ADMIN_USERNAME=admin
   ADMIN_PASSWORD=your_long_unique_password
   PORT=8080
   DATA_DIR=/data
   ```

6. Deploy and visit **`https://tickets.example.com/admin/`**. Your browser prompts for the admin username and password using HTTP Basic authentication. Use HTTPS for the public admin URL.
7. Add a Twenty connection and a route as described below.
8. In Resend, point an `email.received` webhook to **`https://tickets.example.com/webhooks/resend`**.

Coolify's Dockerfile build pack does **not** use the named volume in `compose.yaml`. Add the `/data` mount in the resource's **Persistent Storage** settings and keep that same storage entry attached across redeploys. Recreating the Coolify resource creates a new storage association unless the original volume is explicitly reattached. If the admin configuration unexpectedly appears empty, do not delete or prune volumes: stop the service and recover the old volume containing both `config.db` and `config.key`.

The root `/` redirects to `/admin/`. `GET /healthz` is a public liveness check; configure it on port 8080 in Coolify. The image also includes a Docker health check. Allow at least 25 seconds for graceful shutdown.

## Environment variables

| Variable | Required | Meaning |
| --- | --- | --- |
| `RESEND_API_KEY` | Yes | Permission to retrieve received emails |
| `RESEND_HTTP_TIMEOUT` | No | Received-email API deadline; `30s` by default, positive and at most `45s` |
| `RESEND_WEBHOOK_SECRET` | Yes | This endpoint's Resend signing secret |
| `ADMIN_USERNAME` | Yes | Admin UI username; cannot contain `:` |
| `ADMIN_PASSWORD` | Yes | Admin UI password |
| `PORT` | No | `8080` |
| `DATA_DIR` | No | `./data` natively; `/data` in the Docker image |

**Twenty credentials and inbound addresses are now managed in the UI.** `TWENTY_BASE_URL`, `TWENTY_API_KEY`, and `INBOUND_EMAIL_TO` are no longer used. Changing the admin password does not change the encryption key for saved Twenty credentials.

## Admin UI

### Add a connection

Choose **Add connection** and enter:

- A friendly name, e.g. `Company A`.
- The Twenty instance root URL, e.g. `https://crm.company-a.com`, without `/rest`.
- Its API key.

**Test connection & save / refresh schema** fetches all pages from `/rest/metadata/objects` and saves the connection only after that succeeds. Both the legacy `{data:{objects:[]}}` and newer `{data:[]}` envelopes are supported. The key needs metadata/data-model read access plus read/create permission on the target objects and mapped fields.

Edit an existing connection to rotate its key or refresh cached metadata after changing your Twenty data model. Leave the key blank to retain it; saved keys are never displayed. Keep a connection associated with the same Twenty workspace when rotating keys. Its URL is immutable so pending deliveries cannot be redirected; add a new connection when changing instances.

### Add a route

Choose **Add route**:

1. Select a connection, then **Load objects**.
2. Select an object from its schema, then **Load fields**.
3. Enter a route name and a specific inbound **To** email address.
4. Choose each field's value source, enable the route, and save.

Example:

| Route | Inbound address | Connection | Object |
| --- | --- | --- | --- |
| Company A support | `support@company-a.com` | Company A | `tickets` |
| Company B support | `support@company-b.com` | Company B | `tickets` |

An email matching both routes produces one record per route. Multiple routes can also share one connection. Each route has its own stable record identity; two matching routes intentionally create two records even if they target the same object in the same workspace.

Matching is case-insensitive and compares the complete mailbox. Display names are supported; plus tags and aliases are distinct. CC/BCC, senders, and headers inside forwarded text do not count. Routing uses Resend's outer `to` data, not SMTP-envelope or forwarding-header inference.

### Field mapping

| Source | Behavior |
| --- | --- |
| `default` | Omit the field; Twenty applies its configured default |
| `subject` | Outer email subject, or `(No subject)` when blank |
| `body` | Extracted introductory note and first forwarded/top message |
| `from` | Outer sender string supplied by Resend |
| `email_id` | Resend received-email ID |
| `message_id` | Original email Message-ID |
| `received_at` | Resend creation timestamp; date fields get its date component |
| `fixed` | A configured value validated against the field type |
| `empty` | Explicit JSON null, available only for nullable fields |

For your Ticket object, the form suggests `name → subject` and `issueOrRequest → body`. Leave Status and Type on `default` to use Twenty's settings. The service does not require or automatically send `generated`; Twenty attributes creation to the API token.

Supported typed mappings:

- **Text / rich text:** fixed values or email-derived text. Rich text uses `{ "markdown": "..." }`; Twenty generates its editor representation.
- **Select:** dropdown populated from metadata option values.
- **Multi-select:** a JSON array of valid option values, e.g. `["BUG","REQUEST"]`; available values are shown in the form.
- **Boolean:** true/false control.
- **Number / numeric:** JSON number; integer metadata is validated as an integer.
- **Date / date-time:** `YYYY-MM-DD` / RFC3339; fixed or received date/time.
- **UUID:** a fixed record UUID.
- **Many-to-one relations:** a fixed related-record UUID when metadata supplies `settings.joinColumnName`. For example, an App field maps to `appId`. This version accepts the UUID rather than browsing related records.

System audit fields and ID are excluded. Other composite fields and to-many relationships currently support omission/default only. Unsupported required fields without defaults must be given a default in Twenty before this service can create that object. Server-side validation checks field names/types, select options, required fields, and nullability; the browser cannot substitute arbitrary API field names.

Mappings are cached and snapshotted at intake. Refresh the connection and re-save the route after schema changes. Existing queued snapshots keep their original mapping, so avoid deleting their mapped fields until the backlog is delivered.

### Delivery dashboard

The home page shows connections, routes, a recent activity summary, and the latest 100 saved drafts with per-route pending/retry/delivered state. Disabling a route stops it from matching **new** email; it does not cancel deliveries already accepted for that route.

### Email activity log

Open **Email activity** at `/admin/activity` to search by subject, sender, recipient, or Resend email ID. The list shows enough metadata to identify the email and the latest recorded outcome for intake and each Twenty destination. Click an email to see its stage-by-stage history:

- Webhook received and signature verified
- Recipient routing and durable intake queueing (including ignored emails)
- Resend retrieval, with a running event saved **before** making the request
- Message extraction and manual-review outcomes
- Draft storage and queuing
- Per-destination field mapping and connection loading
- Twenty record lookup, creation, or recovery of an existing record
- Delivery receipt storage, completion, and scheduled retries

**Retrieval failures are visible even when no draft was saved.** Metadata from the verified webhook identifies the email before fetching its body. Separate attempts retain earlier errors after a successful retry. Success in one Twenty instance does not hide failures in another. Outcomes are labeled `running`, `success`, `failure`, `retry`, `review`, or `skipped`, with UTC timestamps and short explanations.

Lists and histories are paginated (50 entries per page); the dashboard previews 10 emails. Refresh the page to see new progress. A `running` event without a subsequent result can also mean the process was interrupted. Activity shows the latest **recorded** outcome; saved drafts/receipts remain the delivery system's authoritative state.

Activity is persisted in `config.db` and survives deploys with the same volume. Email subjects/addresses and error explanations are length-limited; bodies, attachments, field payloads, and API keys are not included. The activity pages require admin authentication. Requests failing signature verification or event validation do not populate the trusted email history.

History starts when this version is deployed; previous console-only failures cannot be reconstructed automatically. Replay a failed Resend event to capture a new attempt. Activity logging is best-effort: if its storage fails, processing continues and `activity log write failed` is written to the application logs. There is no automatic activity retention/deletion policy; include it in normal volume backups and disk-usage management.

## Upgrading from environment-based configuration

1. Keep the existing `/data` volume and note your current Twenty URL/key and inbound address.
2. Add `ADMIN_USERNAME` and `ADMIN_PASSWORD` to Coolify and deploy this version.
3. Open `/admin/`, add the original Twenty connection, retrieve its metadata, and create its route. You can then remove the old Twenty/inbound environment variables.
4. Add the second instance and its route when ready.

While **no routes have been configured**, incoming received-email deliveries return `503` so Resend can retry during setup. Once routes exist, unmatched emails return `204` and are ignored. Replay any failed deliveries that exceeded Resend's retry window, and replay intentionally ignored emails if you later add a route for them.

Previously saved version-1 drafts are not automatically sent to newly added destinations:

- Previously delivered drafts remain delivered and are skipped.
- Pending legacy drafts appear with **Assign legacy draft** on the dashboard. Choose an enabled Tickets route matching the draft's To address and pointing to its **original Twenty workspace**.
- Assignment is one-time and snapshots the route. The original stable ticket ID is reused, so a previous successful creation whose receipt was lost is recovered rather than duplicated.
- `needs_review` records remain available for manual inspection and are not automatically delivered.

The old `check-twenty` CLI command is replaced by the connection form's test/refresh action. There is no automatic import of credentials from the old environment variables.

## Processing and reliability

1. Verify the raw webhook with the official Svix library, including timestamp tolerance.
2. Match enabled routes using the signed event's To recipients when provided. Unmatched events are skipped before fetching content.
3. Commit the event metadata and immutable destination/mapping snapshots to SQLite, then return `204`. Duplicate webhook deliveries resolve to the same queue entry by Resend email ID.
4. A background intake worker fetches `GET https://api.resend.com/emails/receiving/{id}?html_format=cid` independently of the webhook connection. If the signed event omitted To, it routes using the fetched recipients; otherwise it confirms the snapshots against them.
5. Extract the message and atomically save a version-2 draft. Retrieval or storage failures remain queued and retry after 30 seconds, doubling to a 30-minute cap.
6. A separate background delivery worker sends each destination independently, using current credentials for the snapshotted connection.

A `204` acknowledges durable intake or an intentionally ignored event, not retrieval or Twenty delivery. A failure to commit the intake event returns `503`; retrieval failures happen after acknowledgement and retry from SQLite. Missing plain text or empty extraction is saved as `needs_review`. Invalid signatures return `401`; invalid events return `400`; bodies over 1 MiB return `413`.

The intake and delivery workers scan on startup and five seconds after each pass. Both retry schedules survive restarts. Intake retrieval retries indefinitely; destination retries are also independent and indefinite. One failed destination does not discard or repeat successful deliveries to another. Twenty attempts are spaced by two seconds across the delivery worker; each attempt makes at most two Twenty requests. Run one service instance per persistent volume.

Record IDs derive deterministically from route ID + Resend email ID. The worker checks `GET /rest/{plural}/{id}?depth=0`, creates only after `404`, supplies the same ID in POST, and verifies `data.create{Singular}.id`. A lost POST response or local receipt write is recovered by looking up that ID next time. Existing records are not overwritten. A soft-deleted record whose ID remains reserved may need manual restoration.

Resend retrieval has a configurable 30-second timeout; Twenty API requests have a 10-second timeout. Both have a 16 MiB response limit. Metadata refresh has a 45-second overall deadline. No email content or API keys are logged; the admin UI can show subjects to authenticated administrators.

### Diagnosing Resend retrieval timeouts

An error at `stage=resend_fetch` occurs before a draft is saved or any Twenty delivery is attempted. API errors identify whether the request was resolving DNS, connecting, negotiating TLS, waiting for response headers, or reading the response. This can distinguish a slow Resend response from an outbound network problem on the deployment host; increasing the deadline alone does not prove the cause is fixed.

From Coolify's application terminal, run the following with the email ID from the logs:

```sh
twenty-tickets check-resend EMAIL_ID
```

This uses the container's `RESEND_API_KEY` and `RESEND_HTTP_TIMEOUT`, performs the real received-email GET, and reports elapsed time and plain-text size. It does not save a draft, create a ticket, or print email contents/credentials. It can also be run natively with `go run ./cmd/twenty-tickets check-resend EMAIL_ID`.

If it succeeds, a queued event should also succeed on its next automatic attempt. If it still times out, check outbound connectivity to `api.resend.com` on the Coolify server and Resend's service status. HTTP `401`/`403` instead points to the API key/access permissions. The diagnostic command requires only the Resend key/timeout, not admin credentials or storage.

`RESEND_HTTP_TIMEOUT=30s` is the default; you may raise it to `45s` if measured response times justify it. The webhook sender's deadline no longer cancels retrieval because the verified event is committed before the background request starts. A queued event retries automatically after a timeout; manual replay is needed only for failures that happened before this durable-intake version was deployed or before queue commit.

## Persistent data

All state lives under `DATA_DIR`:

```text
/data/config.db                 SQLite connections, routes, pending intake, activity history
/data/config.key                Automatically generated credential-encryption key
/data/<email-hash>.json         Immutable email drafts and destination snapshots
/data/deliveries/*.json         Independent delivery receipts/retry state
/data/assignments/*.json        Explicit legacy-draft destination assignments
```

SQLite uses WAL and FULL synchronous mode. API keys are AES-GCM encrypted in the database with the installation key in `config.key`. The key and database are owner-only files; protection of the complete mounted volume still matters because it contains both. No additional encryption environment variable is needed. An existing database with a missing encryption key fails to open rather than silently replacing the key.

Back up the **whole volume**, including `config.key`. Stop the service before taking a plain filesystem copy so SQLite's database/WAL and file-based drafts are consistent. Losing the key loses access to stored credentials; losing delivery state may cause reprocessing. Keep the volume tied to its original Twenty workspaces. There is no automatic retention policy; manage backups and disk usage. The file store requires hard-link and directory-sync support, as provided by ordinary local Docker volumes.

For Coolify Dockerfile deployments, verify the resource's persistent-storage destination is exactly `/data`. `compose.yaml` is not applied in that deployment mode. Before replacing or recreating a resource, record the volume name shown by Coolify so it can be reattached. The database and key are a pair; attaching or copying only one is not a valid recovery.

## Resend setup

Configure email receiving in Resend, including the required receiving domain/MX records. Create an API key with received-email retrieval permissions (use Full access if restricted keys are sending-only).

In **Resend → Webhooks**, create an endpoint at your public `/webhooks/resend` URL and subscribe to `email.received`. Open the endpoint's details and copy its **Signing Secret** (`whsec_...`) into `RESEND_WEBHOOK_SECRET`. This is separate from your Resend API key. Send a real email to a configured route address to verify the full flow; synthetic test events may reference an email ID that cannot be retrieved.

References: [Resend retrieval](https://resend.com/docs/api-reference/emails/retrieve-received-email), [Webhook verification](https://resend.com/docs/webhooks/verify-webhooks-requests), [Twenty APIs](https://docs.twenty.com/developers/extend/api).

## Message extraction

The parser keeps the sender's introductory note plus the first forwarded message, or the top message for ordinary emails. It recognizes common English Gmail, Outlook, and Apple Mail separators, wrapped `On … wrote:` boundaries, and `>` quoting. Forwarding headers and older history are removed; signatures inside the selected message are retained.

This is heuristic parsing. Localized markers, inline/bottom-posted replies, and prose resembling mail headers can need adjustments. Original plain text is retained for later reprocessing. HTML-only messages are saved for review by ID/metadata; HTML, raw MIME, and attachments are not downloaded or stored. Markdown-like text may render as formatting in Twenty.

## Local execution and verification

Use Go 1.26 or newer. Copy `.env.example` to `.env` and fill in credentials. Compose reads `.env` automatically:

```sh
docker compose up --build -d
curl -f http://localhost:8080/healthz
# Open http://localhost:8080/admin/ locally.
docker compose logs -f
```

For native execution, export the environment variables in your shell first; the binary does not load dotenv files:

```sh
go run ./cmd/twenty-tickets
```

The named Compose `inbox` volume survives container replacement. `docker compose down -v` deletes it. A public HTTPS endpoint/tunnel is required for Resend to reach a local instance.

```sh
go test -race ./...
go vet ./...
docker build -t twenty-tickets .
```

Tests exercise authenticated admin forms and activity views, CSRF protection, pre-draft failure history, cancellation-safe activity recording, search/pagination, schema rendering, metadata pagination/envelopes, encrypted credential persistence, typed mappings, recipient routing, signed webhooks, independent retries, lost responses, failed receipt writes, legacy assignment, parser fixtures, and duplicate handling. Live instance credentials are needed to confirm workspace-specific permissions and schema behavior.
