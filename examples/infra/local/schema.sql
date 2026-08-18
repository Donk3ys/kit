-- The example service's only table.
--
-- This file is the single source of truth for the schema. Docker Compose mounts
-- it into the Postgres image's initdb directory, so what you run and what the
-- docs claim cannot drift apart -- which they previously had.
--
-- UNIQUE on name is load-bearing, not decoration. pgStore.Create checks for a
-- duplicate with a SELECT inside pg.InTx before inserting, which narrows the
-- race but does not close it: under READ COMMITTED two concurrent creates can
-- both see "no such name" and both insert. The constraint is what actually
-- keeps the data right under load.
--
-- Keeping the *response* right takes a second thing, and this comment used to
-- claim the constraint alone did it. It does not: the constraint raises
-- SQLSTATE 23505 on constraint "widgets_name_key", and unless pgStore.Create
-- translates that back to errNameTaken it reaches main.go's storage branch and
-- the loser of the race is told 503 rather than 409. Renaming this constraint
-- breaks that translation, so change it in store.go in the same edit.
CREATE TABLE IF NOT EXISTS widgets (
    id       UUID PRIMARY KEY,
    name     TEXT NOT NULL UNIQUE,
    quantity INT  NOT NULL
);
