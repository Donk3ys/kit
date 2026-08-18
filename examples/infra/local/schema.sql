-- The example service's only table.
--
-- This file is the single source of truth for the schema. Docker Compose mounts
-- it into the Postgres image's initdb directory, so what you run and what the
-- docs claim cannot drift apart -- which they previously had.
--
-- UNIQUE on name is load-bearing, not decoration. pgStore.Create checks for a
-- duplicate with a SELECT inside db.InTx before inserting, which narrows the
-- race but does not close it: under READ COMMITTED two concurrent creates can
-- both see "no such name" and both insert. The constraint is what actually
-- makes errNameTaken true, and without it the 409 path is a lie under load.
CREATE TABLE IF NOT EXISTS widgets (
    id       UUID PRIMARY KEY,
    name     TEXT NOT NULL UNIQUE,
    quantity INT  NOT NULL
);
