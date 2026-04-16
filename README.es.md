# pg2json

Driver especializado en el **read-path** de PostgreSQL para Go.
Convierte un `SELECT` en cualquiera de estas formas, con allocs
mínimos y sin tipos Go intermedios:

- Bytes JSON (array / NDJSON / columnar / TOON)
- Structs Go tipadas (`ScanStruct[T]`)
- Filas compatibles con `database/sql` (drop-in para código existente)
- Callbacks por batch con memoria acotada (`ScanStructBatched[T]`)

Escrituras, transacciones, LISTEN/NOTIFY, COPY, replicación —
**fuera de alcance**. Si tu app también hace esas cosas, seguí con tu
driver actual para ellas; pg2json corre al lado sin interferir.

Disponible en [English](README.md) · [Español](README.es.md).

---

## Instalación

```bash
go get github.com/arturoeanton/pg2json
```

Requiere Go 1.21+. Sin cgo. Cero dependencias fuera de la stdlib en
la librería núcleo.

---

## Dos paths de API — elegí el que te encaje

pg2json expone el mismo decoder a través de dos front-ends. Podés
mezclarlos en el mismo programa.

### API nativa (más rápida, read-optimised)

```go
import (
    "context"
    "database/sql"
    "encoding/json"

    "github.com/arturoeanton/pg2json/pg2json"
)

cfg, _ := pg2json.ParseDSN("postgres://user:pass@host/db?sslmode=require")
c, err := pg2json.Open(context.Background(), cfg)
if err != nil { log.Fatal(err) }
defer c.Close()

// 1) Bytes JSON directos — bufferizado
buf, err := c.QueryJSON(ctx,
    "SELECT id, name, meta FROM users WHERE id = $1", 42)
// buf == [{"id":42,"name":"alice","meta":{"k":42}}]

// 2) JSON streameado a cualquier io.Writer
err = c.StreamNDJSON(ctx, w,
    "SELECT id, name FROM users WHERE active = $1", true)

// 3) Scan tipado a struct
type User struct {
    ID    int32
    Name  string
    Email sql.NullString       // columna nullable
    Tags  []string              // text[]
    Meta  json.RawMessage       // jsonb
}
users, err := pg2json.ScanStruct[User](c, ctx,
    "SELECT id, name, email, tags, meta FROM users")

// 4) Scan por batch — O(batchSize) en memoria para 100M de filas
err = pg2json.ScanStructBatched[User](c, ctx, /*batch*/ 10_000,
    func(batch []User) error {
        return processUserBatch(batch) // llamado hasta consumir todo
    },
    "SELECT id, name, email, tags, meta FROM user_log")
```

### Adapter `database/sql` (drop-in para código existente)

```go
import (
    "database/sql"
    _ "github.com/arturoeanton/pg2json/pg2json/stdlib" // registra "pg2json"
)

db, err := sql.Open("pg2json",
    "postgres://user:pass@host/db?sslmode=require")
if err != nil { log.Fatal(err) }
defer db.Close()

rows, err := db.Query(
    "SELECT id, name, email FROM users WHERE active = $1", true)
for rows.Next() {
    var id int32
    var name string
    var email sql.NullString
    rows.Scan(&id, &name, &email)
    // ...
}
rows.Close()
```

El adapter es **read-only**: `db.Exec`, `db.Begin`, `db.BeginTx`
devuelven un error explícito. Hacé las escrituras con tu driver
actual — un segundo `*sql.DB` al mismo Postgres convive sin problemas.

---

## Modos de salida (sólo native)

| método | forma | consumidor típico |
|---|---|---|
| `QueryJSON(ctx, sql, args...) ([]byte, error)` | `[{...},{...}]` bufferizado | body REST < 10 MB |
| `StreamJSON(ctx, w, sql, args...)` | `[{...},{...}]` streameado | respuesta HTTP, resultados grandes |
| `StreamNDJSON(ctx, w, sql, args...)` | `{...}\n{...}\n` | consumidores por línea (jq, Kafka, S3 NDJSON) |
| `StreamColumnar(ctx, w, sql, args...)` | `{"columns":[...],"rows":[[...],[...]]}` | grids, planillas, UIs estilo `ag-grid` |
| `StreamTOON(ctx, w, sql, args...)` | `[?]{col,col}\nval,val\n` | pipelines LLM / agent con presupuesto de tokens |

Cada modo flushea incremental por dos triggers: umbral de bytes
(`Config.FlushBytes`, default 32 KiB) y tiempo transcurrido
(`Config.FlushInterval`, 0 = desactivado). El header se **difiere
hasta la primera DataRow**, así una query que falla antes de
cualquier fila produce cero bytes downstream.

---

## Tipos soportados

Se pide formato binario para cada OID que tiene decoder
especializado; el texto es el fallback correcto.

| PostgreSQL | Wire | JSON | Struct target |
|---|---|---|---|
| bool | binary | `true` / `false` | `bool` |
| int2, int4, int8, oid | binary | número | `int16/32/64/int`, `uint32` (OID) |
| float4, float8 | binary | número (NaN → `"NaN"`) | `float32/64` |
| numeric | text | número | `string` (o `sql.Scanner`) |
| text, varchar, bpchar, name | text | string escapado | `string`, `[]byte` |
| uuid | binary | `"xxxxxxxx-..."` | `string`, `[16]byte` |
| json, jsonb | binary | JSON embebido | `json.RawMessage`, `[]byte`, `string` |
| bytea | binary | `"\\x<hex>"` | `[]byte` |
| date, time, timetz | binary | string ISO | `time.Time` |
| timestamp, timestamptz | binary | string ISO 8601 | `time.Time` |
| interval | binary | duración ISO 8601 | `string` |
| arrays (1-D, 2-D) | binary | arrays JSON anidados | `[]T` / `[][]T` de los anteriores |
| ranges (`int4range`, `tstzrange`, …) | binary | string quoteado (paths JSON) | `pg2json.RangeBytes` |
| cualquier otro | text | string escapado (correcto) | `string` |

`NULL` se emite como JSON `null` o, en struct scan, zero-value /
puntero nil / `sql.Null*.Valid=false`. Los tipos custom que
implementan `sql.Scanner` se routean por `Scan(any)` con el
intermedio canónico de `database/sql` — `uuid.UUID`,
`decimal.Decimal` y tus dominios propios funcionan sin cableado
específico de pg2json.

También soportados en struct scan:

- **Embedded structs** — campos anónimos se aplanan siguiendo la
  regla de promoción propia de Go (el outer sombrea duplicados
  embebidos).
- **Arrays 2-D** — `[][]int32` etc. desde `int4[][]` y similares.
  Targets 3-D+ fallan rápido con mensaje claro.
- **Range types** — `int4range` / `int8range` / `numrange` /
  `tsrange` / `tstzrange` / `daterange` vía el target
  `pg2json.RangeBytes`. El struct expone los bytes crudos de cada
  bound más las flags (empty, inclusive, infinite en cada lado).
  Decodificá los bounds con `pg2json.DriverValueDecoder` sobre el
  OID del elemento.

No soportados en struct scan (usá tu otro driver o un `sql.Scanner`
custom): composite types (`ROW(a,b,c)`), arrays 3-D+, wrappers
`pgtype.*`. Los paths JSON sí los manejan seguros vía fallback
text.

---

## Performance

Apple M4 Max, PostgreSQL 17 en Docker sobre loopback, queries de
100 000 filas. Medianas de 3 corridas. Bench completo en
`tests/full_compare_test.go`.

Forma: `bench_mixed_5col` — `id int4, name text, score float8, flag
bool, meta jsonb`.

| path | ns/op | allocs/op | bytes/op |
|---|---|---|---|
| **pg2json** | | | |
| `StreamNDJSON` | 32.0 ms | **5** | **82 KB** |
| `StreamTOON` | 31.8 ms | 5 | 82 KB |
| `StreamColumnar` | 32.0 ms | 6 | 82 KB |
| `QueryJSON` (bufferizado) | 32.0 ms | 30 | 53 MB |
| `ScanStruct[T]` | 31.2 ms | 200 k | 20 MB |
| `ScanStructBatched[T]` (10 k) | 30.9 ms | 200 k | **3.8 MB** |
| `stdlib.Query` | 31.7 ms | 800 k | 12 MB |
| **Referencia: otros drivers Go** | | | |
| pgx naive (Map + `json.Marshal`) | 121 ms | 3.3 M | 155 MB |
| pgx `RawValues` + NDJSON a mano | 30.5 ms | 11 | 66 KB |
| pgx `Scan(&f...)` manual | 30.3 ms | 500 k | 26 MB |
| pgx `CollectRows[ByName]` | 45 ms | 700 k | 70 MB |
| pgx `stdlib.Query` | 30.1 ms | 900 k | 13.5 MB |

Notas:
- Sobre loopback, el decoder cliente es una fracción chica del
  tiempo total (~2 ms de ~32 ms). Las diferencias entre drivers se
  diluyen aún más sobre LAN / WAN donde el wire y el server
  dominan.
- Zero-alloc per celda está impuesto en el hot path de JSON por un
  bench que corre en cada cambio en `internal/rows` o
  `internal/types`.
- La columna `bytes/op` para `ScanStructBatched` muestra el
  comportamiento memory-bounded: las mismas 100 k filas caben en un
  buffer rodante de 3.8 MB sin importar el total.

Corré el bench vos:

```bash
docker compose -f docker/docker-compose.yml up -d
export PG2JSON_TEST_DSN="postgres://pgopt:pgopt@127.0.0.1:55432/pgopt?sslmode=disable"
go test ./tests -run '^$' -bench BenchmarkFullCompare -benchmem -benchtime=2s -count=3
```

---

## Puesta en producción

La librería está pensada para **gateways de datos** frente a Citus +
PgBouncer en modo transacción. Cada feature de hardening existe por
un modo de falla específico de ese stack.

```go
p, _ := pool.New(pool.Config{
    Config: pg2json.Config{
        Host: "pgbouncer", Port: 6432,
        Database: "app", User: "gateway",
        MaxResponseBytes:     16 << 20,         // 16 MB por query
        MaxResponseRows:      100_000,          // tope de filas
        DefaultQueryTimeout:  10 * time.Second, // default capa 2
        FlushInterval:        100 * time.Millisecond,
        SlowQueryThreshold:   250 * time.Millisecond,
        RetryOnSerialization: true,             // rebalance Citus
        Keepalive:            30 * time.Second,
    },
    MaxConns:           32,
    MaxInFlightBuffers: 256,
})
defer p.Close()
```

Features clave para producción:

- **Topes duros por respuesta** (`MaxResponseBytes` /
  `MaxResponseRows`) abortan con `CancelRequest` server-side y
  devuelven `*ResponseTooLargeError` indicando si ya se habían
  flusheado bytes.
- **Retry transparente** en SQLSTATE 26000 (rotación de backend por
  PgBouncer-txn) y opt-in en 40001 / 40P01 (shard rebalance Citus).
- **`CancelRequest` real** en `ctx.Cancel` — el backend deja de
  producir filas inmediatamente, no después de drenar buffers.
- **Header diferido** — una query que falla antes de cualquier
  DataRow escribe cero bytes downstream, así las respuestas HTTP
  quedan limpias.
- **Shutdown graceful** — `Pool.Drain(ctx)` deja de aceptar
  Acquires, `WaitIdle(ctx)` espera sin bloquear traffic nuevo.
- **Observer** con `OnQueryStart/End/Slow/Notice` + contadores
  atómicos vía `Stats()`.
- **TCP keepalive**, perillas de socket buffers (`TCPRecvBuffer`,
  `TCPSendBuffer`), bufio read buffer configurable.

---

## Cuándo usarlo

| caso de uso | fit |
|---|---|
| Endpoint HTTP que devuelve filas como JSON | native `StreamNDJSON` |
| Export NDJSON / S3 / Kafka | native `StreamNDJSON` |
| Server-Sent Events, long-polling | native + `FlushInterval` |
| LLM / agent con presupuesto de tokens | native `StreamTOON` |
| Read-path en código `database/sql` existente | adapter (cambio de 1 línea) |
| Bulk read que OOMearía con un slice | `ScanStructBatched[T]` |
| Leer filas y forwardearlas a HTTP / queue / cache | cualquiera de los dos paths |
| Writes, transacciones, LISTEN/NOTIFY, COPY | seguí con tu driver actual |
| Composite (`ROW(…)`) o arrays 3-D+ | seguí con tu driver actual |

---

## Ejemplos

### Endpoint REST → stream NDJSON

```go
func listUsers(p *pool.Pool) http.HandlerFunc {
    return func(w http.ResponseWriter, r *http.Request) {
        c, err := p.Acquire(r.Context())
        if err != nil { http.Error(w, err.Error(), 503); return }
        defer c.Release()

        w.Header().Set("Content-Type", "application/x-ndjson")
        _ = c.StreamNDJSON(r.Context(), w,
            "SELECT id, email, created_at FROM users WHERE active = $1",
            true)
    }
}
```

### Export bulk con memoria acotada

```go
func exportActiveUsers(ctx context.Context, c *pg2json.Client,
    out io.Writer) error {
    return pg2json.ScanStructBatched[User](c, ctx, 5_000,
        func(batch []User) error {
            for _, u := range batch {
                b, _ := json.Marshal(u)
                if _, err := fmt.Fprintln(out, string(b)); err != nil {
                    return err
                }
            }
            return nil
        },
        "SELECT id, name, email, created_at FROM users")
}
```

### Setup con dos pools (lectura + escritura)

```go
var (
    reads  *pg2json.Pool  // todos los SELECTs
    writes *pgxpool.Pool  // INSERT / UPDATE / DELETE / transacciones
)

func getUser(ctx context.Context, id int64) (User, error) {
    c, _ := reads.Acquire(ctx); defer c.Release()
    users, err := pg2json.ScanStruct[User](c, ctx,
        "SELECT id, name, email FROM users WHERE id = $1", id)
    if err != nil || len(users) == 0 {
        return User{}, err
    }
    return users[0], nil
}

func createOrder(ctx context.Context, o Order) (int64, error) {
    tx, err := writes.Begin(ctx)
    if err != nil { return 0, err }
    defer tx.Rollback(ctx)
    var id int64
    err = tx.QueryRow(ctx,
        `INSERT INTO orders (...) VALUES (...) RETURNING id`,
        o.Args()...).Scan(&id)
    if err != nil { return 0, err }
    return id, tx.Commit(ctx)
}
```

Mismo Postgres, dos pools, separación de responsabilidades clara.

### Embedded structs, arrays 2-D, range types

```go
type Audit struct {
    CreatedAt time.Time
    UpdatedAt time.Time
}
type Product struct {
    Audit                   // aplanado — rows con created_at / updated_at
    ID    int64
    Name  string
    Sizes [][]int32         // array 2-D: grid de talles disponibles
    Span  pg2json.RangeBytes `pg2json:"availability"` // columna tstzrange
}

prods, err := pg2json.ScanStruct[Product](c, ctx, `
    SELECT id, name, sizes, availability, created_at, updated_at
    FROM products WHERE active = $1`, true)

// prods[0].Audit.CreatedAt ← desde created_at
// prods[0].Sizes           ← [[S,M],[L,XL]] como [][]int32
// prods[0].Span.Empty      ← true si el range es vacío
// prods[0].Span.Lower      ← bytes wire crudos del lower
// prods[0].Span.Upper      ← bytes wire crudos del upper
```

Decodificá un bound más allá con el helper por OID:

```go
// Para un tstzrange:
decode := pg2json.DriverValueDecoder(/*tstz*/ 1184, /*binary*/ 1)
lower := decode(prods[0].Span.Lower).(time.Time)
```

### Drop-in para código generado por `sqlc` o legacy

```go
// Antes (cualquier driver registrado como "postgres"):
db, _ := sql.Open("postgres", dsn)

// Después:
import _ "github.com/arturoeanton/pg2json/pg2json/stdlib"
db, _ := sql.Open("pg2json", dsn)

// Todas las llamadas existentes a db.Query / db.QueryRow /
// db.QueryContext / db.Prepare siguen funcionando. db.Exec /
// db.Begin devuelven un error read-only claro, que es la señal
// para routear las escrituras por tu driver de escritura.
```

---

## Limitaciones — la lista honesta

- **Read-only por diseño.** INSERT / UPDATE / DELETE / LISTEN /
  NOTIFY / COPY / replicación se rechazan en la API o no están
  implementados. Mantené un driver de escritura para esos.
- **Alcance del struct scan.** Cubre scalares, pointer-for-NULL,
  `sql.Null*`, cualquier `sql.Scanner`, arrays 1-D y 2-D, embedded
  structs, y range types (via `pg2json.RangeBytes`). Composite
  types (`ROW(a,b)`), arrays 3-D+, y wrappers `pgtype.*` no están
  en alcance — usá el scan de pgx para esas formas.
- **Numeric usa formato texto.** El decoder numeric binary se
  incluye como utilidad opt-in (`types.EncodeNumericBinary`) pero
  no se selecciona por default; sobre loopback el path text es más
  rápido.
- **Sin soak test de 8 horas / 500k usuarios aún.** Benches, fuzz y
  suite de integración cubren correctness bajo bursts, pero falta
  validación de larga duración en el mundo real. Anclá a `v0.x`.
- **COPY binary export en el roadmap, no implementado.** Vencería
  al extended-query path en bulk exports donde el caller puede
  garantizar SQL confiable (COPY no acepta bind parameters).

---

## Layout

```
pg2json/
├── pg2json/                 # API pública (native)
│   ├── conn.go query.go stream.go config.go errors.go
│   ├── scan.go              # ScanStruct[T] + ScanStructBatched[T]
│   ├── iter.go              # RawQuery / Iterator para DataRow lazy
│   ├── stmtcache.go         # cache de prepared statements per-conn
│   ├── ctx.go               # watcher ctx.Cancel → CancelRequest
│   ├── observer.go          # hook de telemetría + contadores atómicos
│   ├── stdlib/              # driver database/sql ("pg2json")
│   └── pool/                # pool de conexiones + Drain / WaitIdle
├── internal/
│   ├── wire/                # reader/writer framed sobre net.Conn
│   ├── protocol/            # códigos de mensaje + OIDs
│   ├── auth/                # MD5 + SCRAM-SHA-256 (solo stdlib)
│   ├── rows/                # compilación de plan + hot loop de DataRow (JSON + TOON)
│   ├── types/               # encoders text + binary per OID; arrays; interval; numeric
│   ├── jsonwriter/          # appender directo a []byte + tabla de escape
│   ├── bufferpool/          # sync.Pool de []byte
│   └── pgerr/               # decoder de ErrorResponse / NoticeResponse
├── docker/                  # docker-compose + tablas seedeadas para bench
├── cmd/
│   ├── pg2json_demo/        # CLI: SELECT → JSON (PG2JSON_DSN)
│   └── pg2json_bench/       # harness de perf end-to-end
└── tests/                   # integration, comparison, scan, TOON, iter, stdlib
```

Ver [`ARCHITECTURE.md`](ARCHITECTURE.md) para el rationale de diseño
y [`ROADMAP.md`](ROADMAP.md) para trabajo en vuelo y futuro.

---

## Build tags

- `pg2json_simd` — implementación SWAR (SIMD-Within-A-Register) del
  escape de strings JSON. Go puro, sin assembly. Medido ~4× más
  rápido en strings ASCII medianas/largas. Desactivado por default.

```bash
go build -tags pg2json_simd ./...
```

---

## Licencia

Ver [`LICENSE`](LICENSE).
