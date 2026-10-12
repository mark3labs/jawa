# Submit CLI configuration

Run `go run ./cmd/submit -text 'Task instructions'`. The command loads `.env`
using its existing loader (existing shell values win), then resolves configuration
into local values. CLI overrides never mutate the process environment.

| Flag | Environment default | Built-in default |
| --- | --- | --- |
| `-root` | `JAWA_NATS_ROOT_SUBJECT` | `bonnie` |
| `-nats-url` | `NATS_URL` | `nats://127.0.0.1:4222` |
| `-nats-username` | `NATS_USERNAME` | empty |
| `-nats-password` | `NATS_PASSWORD` | empty |

Explicit flags override environment defaults, including empty credentials.
All runtime environment variables consumed by submit now have equivalent flags.
The existing `-id`, `-text`, `-agent`, and `-timeout` flags remain available; they
have no environment defaults. Submit does not select an agent model.

Connection defaults are hidden from help because URLs may embed credentials.
Connection errors report a generic failure rather than echoing credential-bearing
URLs or library errors. Prefer environment credentials: CLI secrets can still be
visible in shell history or process listings.
