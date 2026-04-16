-- Benchmark data for pg2json vs pgx comparisons.
-- Five shapes × 100k rows each, materialised so the server spends the
-- same time on I/O and the driver-level delta is what we actually measure.

SET synchronous_commit = off;

CREATE TABLE bench_narrow_int (
    id int4 PRIMARY KEY
);
INSERT INTO bench_narrow_int (id)
SELECT g FROM generate_series(1, 100000) g;

CREATE TABLE bench_mixed_5col (
    id    int4 PRIMARY KEY,
    name  text    NOT NULL,
    score float8  NOT NULL,
    flag  boolean NOT NULL,
    meta  jsonb   NOT NULL
);
INSERT INTO bench_mixed_5col (id, name, score, flag, meta)
SELECT g,
       'name-' || g,
       (g::float8 / 3.0),
       (g % 2 = 0),
       jsonb_build_object('k', g)
FROM generate_series(1, 100000) g;

CREATE TABLE bench_wide_jsonb (
    id int4 PRIMARY KEY,
    j  jsonb NOT NULL
);
INSERT INTO bench_wide_jsonb (id, j)
SELECT g, '{"a":1,"b":[1,2,3],"c":"x"}'::jsonb
FROM generate_series(1, 100000) g;

CREATE TABLE bench_array_int (
    id  int4 PRIMARY KEY,
    arr int4[] NOT NULL
);
INSERT INTO bench_array_int (id, arr)
SELECT g,
       ARRAY[g, g+1, g+2, g+3, g+4, g+5, g+6, g+7, g+8, g+9]::int4[]
FROM generate_series(1, 100000) g;

CREATE TABLE bench_null_heavy (
    id int4 PRIMARY KEY,
    v  text
);
INSERT INTO bench_null_heavy (id, v)
SELECT g,
       CASE WHEN g % 2 = 0 THEN NULL ELSE 'row-' || g END
FROM generate_series(1, 100000) g;

-- Numeric-heavy table to exercise the new binary numeric decoder path.
CREATE TABLE bench_numeric (
    id    int4 PRIMARY KEY,
    price numeric(18,4) NOT NULL,
    rate  numeric(10,6) NOT NULL
);
INSERT INTO bench_numeric (id, price, rate)
SELECT g,
       (g::numeric / 100.0)::numeric(18,4),
       ((g % 9973)::numeric / 1000000.0)::numeric(10,6)
FROM generate_series(1, 100000) g;

ANALYZE;
