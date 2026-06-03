/****************************************************************
 *
 * One-off migration: add-to batch provenance
 *
 * Brings an existing fmsgd database up to the current dd.sql schema:
 * introduces msg_add_to_batch / msg_add_to.batch_id and removes the legacy
 * single msg.add_to_from column. A flat msg_add_to list with one set-once
 * add_to_from could not preserve which sender added which recipients, nor
 * when -- this backfills one synthetic batch per message so that history is
 * (approximately) recoverable.
 *
 * Safe to re-run: every step is guarded, so a second run (or a database that
 * already has the new shape) is a no-op. Run msg_add_to_batch's CREATE from
 * dd.sql first if it does not yet exist
 *
 * Apply with: psql "<conn>" -f temp.sql
 *
 ****************************************************************/

-- add column before its index so an existing msg_add_to (created before
-- batch_id) is altered first.
alter table msg_add_to add column if not exists batch_id bigint references msg_add_to_batch (id);
create index if not exists msg_add_to_batch_id_idx on msg_add_to (batch_id);

do $$
begin
    -- Backfill one synthetic batch per message that already has add-to
    -- recipients, sourcing the sender from the legacy column, then drop it.
    if exists (
        select 1 from information_schema.columns
        where table_name = 'msg' and column_name = 'add_to_from'
    ) then
        insert into msg_add_to_batch (msg_id, add_to_from, time_added)
        select m.id, coalesce(nullif(m.add_to_from, ''), m.from_addr), coalesce(m.time_sent, 0)
        from msg m
        where exists (
            select 1 from msg_add_to a where a.msg_id = m.id and a.batch_id is null
        );

        update msg_add_to a
        set batch_id = b.id
        from msg_add_to_batch b
        where a.batch_id is null and b.msg_id = a.msg_id;

        alter table msg drop column add_to_from;
    end if;

    -- Tighten the FK to match dd.sql once every recipient is linked.
    if not exists (select 1 from msg_add_to where batch_id is null) then
        alter table msg_add_to alter column batch_id set not null;
    end if;
end $$;
