-- PostgreSQL data definition for fmsgd.
--
-- This script is IDEMPOTENT: every statement is safe to re-run (create
-- table/index if not exists, create or replace function, drop trigger if
-- exists before create trigger), so deploys re-run the whole script:
--
--   psql -d fmsgd -v ON_ERROR_STOP=1 -f dd.sql
--
-- Keep it that way.

create table if not exists msg (
    id            	bigserial       	primary key,
	version			int					not null,
    pid           	bigint          	references msg (id),
	no_reply		boolean				not null default false,
	is_important	boolean				not null default false,
	is_deflate		boolean				not null default false,
	is_terminal		boolean				not null default false, -- SPEC §3 bit 6: leaf message, nothing may reference it via pid
    time_sent     	double precision,             -- time sending host recieved message for sending, message timestamp field, NULL means message not ready for sending i.e. draft
    from_addr     	varchar(255)    	not null,
    topic         	varchar(255)    	not null,
    type          	varchar(255)    	not null,
    sha256        	bytea           	unique,
    psha256       	bytea,
	size			int					not null, -- spec allows uint32 but we don't enforced by FMSG_MAX_MSG_SIZE
    filepath      	text            	not null,
    wire_header   	bytea,                        -- exact protocol header (fields 1-13)
    wire_message    jsonb                         -- durable original wire representation; null for drafts or originals received only through add-to
);
create index if not exists msg_lower_idx on msg ((lower(from_addr)));

create table if not exists msg_to (
	id				bigserial			primary key,
	msg_id			bigint				not null references msg (id),
	addr			varchar(255)		not null,
    time_delivered  double precision,   -- if sending, time sending host recieved delivery confirmation, if receiving, time successfully received message
    time_last_attempt double precision, -- only used when sending, time of last delivery attempt if failed; otherwise null
    time_read       double precision,   -- time recipient read the message; null if unread
    response_code   smallint,		    -- when sending, response code of last delivery attempt if failed; when receiving, the per-recipient code this host responded, or a negative local sentinel (-1 attempt got no response, retryable; -2 recorded from an exchange, another host's delivery)
    attempt_count   int             not null default 0, -- number of failed delivery attempts; used for exponential back-off
	unique (msg_id, addr)
);
create index if not exists msg_to_lower_idx on msg_to ((lower(addr)));

-- Each add-to delivery for a shared message is one batch: a single sender
-- (add_to_from) added a set of recipients at a point in time. Storing batches
-- separately lets readers reconstruct who added which recipients and when,
-- which a single flat recipient list cannot preserve (SPEC §12). A batch's
-- identity is its message hash (sha256), which covers the batch's time: the
-- same addresses re-issued at a new time are a distinct batch, not a
-- duplicate (SPEC §11/§12). Batches of a draft finalize when it is sent.
create table if not exists msg_add_to_batch (
	id				bigserial			primary key,
	msg_id			bigint				not null references msg (id),
	add_to_from		varchar(255)		not null,           -- sender that added this batch's recipients
	time_added		double precision	not null,           -- the batch message's wire time field (for locally originated batches, when the batch was created)
	sha256			bytea,                                  -- finalized batch identity (SPEC §11)
    wire_message    jsonb                                   -- durable batch wire representation
);
create index if not exists msg_add_to_batch_msg_id_idx on msg_add_to_batch (msg_id);

create table if not exists msg_add_to (
	id				bigserial			primary key,
	msg_id			bigint				not null references msg (id),
	batch_id		bigint				not null references msg_add_to_batch (id), -- batch this recipient was added in
	addr			varchar(255)		not null,
    time_delivered  double precision,   -- if sending, time sending host recieved delivery confirmation, if receiving, time successfully received message
    time_last_attempt double precision, -- only used when sending, time of last delivery attempt if failed; otherwise null
    time_read       double precision,   -- time recipient read the message; null if unread
    response_code   smallint,		    -- when sending, response code of last delivery attempt if failed; when receiving, the per-recipient code this host responded, or a negative local sentinel (-1 attempt got no response, retryable; -2 recorded from an exchange, another host's delivery)
    attempt_count   int             not null default 0, -- number of failed delivery attempts; used for exponential back-off
	unique (batch_id, addr)
);
create index if not exists msg_add_to_lower_idx on msg_add_to ((lower(addr)));
create index if not exists msg_add_to_batch_id_idx on msg_add_to (batch_id);

create table if not exists msg_attachment (
    msg_id        	bigint          references msg (id),
    position      	smallint        not null default 0,
    flags         	smallint        not null default 0,
    type          	varchar(255)    not null default 'application/octet-stream',
    filename      	varchar(255)    not null,
    filesize      	int             not null, 
    filepath      	text			not null,
    primary key (msg_id, filename)
);

-- Sender-side state for add-to participant notification (SPEC §10.2): an
-- add-to message is sent to every participant domain of the message being
-- added to -- the domains of from and every to address as well as the new
-- recipients' -- so all participants learn recipients were added, not only
-- the domains hosting the new recipients. Domains hosting a recipient of the
-- batch itself learn through normal recipient delivery; every other
-- participant domain gets one row here per batch and receives the add-to as
-- a notification-only exchange completing at code 11. Rows are created by
-- the Web API when recipients are added through it (the local domain itself
-- needs no row -- this database is its record).
create table if not exists msg_add_to_notify (
    id                bigserial        primary key,
    batch_id          bigint           not null references msg_add_to_batch (id),
    domain            varchar(255)     not null,
    time_notified     double precision,   -- time remote host acknowledged the batch; null means pending
    time_last_attempt double precision,   -- time of last failed attempt; drives exponential back-off
    response_code     smallint,           -- response code of last attempt
    attempt_count     int              not null default 0,
    unique (batch_id, domain)
);

create index if not exists msg_add_to_batch_sha256_idx on msg_add_to_batch (sha256) where sha256 is not null;
create index if not exists msg_pid_idx on msg (pid) where pid is not null;

-- Functions and triggers.

-- keep protocol parent hash populated for locally-created replies that set
-- the relational parent id. A reply cannot reference a draft parent or a
-- terminal parent (SPEC v0.6.0 §3: a Sending Host must not transmit a reply
-- to a terminal message, so refuse to create one), and any explicit psha256
-- must match the referenced parent's sha256.
create or replace function populate_msg_psha256_from_pid() returns trigger as $$
declare
    parent_time_sent double precision;
    parent_sha256 bytea;
    parent_is_terminal boolean;
begin
    if NEW.pid is null then
        return NEW;
    end if;

    select parent.time_sent, parent.sha256, parent.is_terminal
    into parent_time_sent, parent_sha256, parent_is_terminal
    from msg parent
    where parent.id = NEW.pid;

    if not found then
        raise exception 'parent message % does not exist', NEW.pid;
    end if;

    if parent_time_sent is null then
        raise exception 'cannot set pid %: parent message is a draft', NEW.pid;
    end if;

    if parent_is_terminal then
        raise exception 'cannot set pid %: parent message is terminal', NEW.pid;
    end if;

    if parent_sha256 is null or octet_length(parent_sha256) <> 32 then
        raise exception 'parent message % has no finalized identity', NEW.pid;
    end if;

    if NEW.psha256 is null or octet_length(NEW.psha256) = 0 then
        NEW.psha256 = parent_sha256;
    elsif NEW.psha256 <> parent_sha256 then
        -- a reply may reference one of the parent's add-to batch messages by
        -- its batch hash (SPEC §12); the relational parent is the shared row
        if not exists (
            select 1 from msg_add_to_batch b
            where b.msg_id = NEW.pid and b.sha256 = NEW.psha256
        ) then
            raise exception 'psha256 does not match parent message % sha256 or any of its add-to batch hashes', NEW.pid;
        end if;
    end if;

    return NEW;
end;
$$ language plpgsql;

drop trigger if exists trg_msg_populate_psha256 on msg;
create trigger trg_msg_populate_psha256
    before insert or update of pid, psha256 on msg
    for each row execute function populate_msg_psha256_from_pid();

-- recipients cannot be added to a terminal message (SPEC §12): refuse to
-- create a batch for one, so the sender never has such a unit to transmit.
create or replace function prevent_add_to_terminal_msg() returns trigger as $$
begin
    if exists (select 1 from msg where id = NEW.msg_id and is_terminal) then
        raise exception 'cannot add recipients to message %: it is terminal', NEW.msg_id;
    end if;
    return NEW;
end;
$$ language plpgsql;

drop trigger if exists trg_msg_add_to_batch_terminal on msg_add_to_batch;
create trigger trg_msg_add_to_batch_terminal
    before insert on msg_add_to_batch
    for each row execute function prevent_add_to_terminal_msg();

-- Notify the sender's outgoing worker (channel new_msg_to) whenever new
-- delivery work appears. One function serves all three triggers, dispatching
-- on the table it fired for:
--   * msg               -- a draft message transitions to sent (time_sent set
--                          for the first time); notify every recipient.
--   * msg_to/msg_add_to -- a recipient row is inserted against an already-sent
--                          message (recipients added via add-to after the
--                          message was sent, including a freshly inserted
--                          message whose recipient rows follow in the same
--                          transaction); notify that recipient.
-- The payload is advisory only: the worker re-polls fully on any wake-up.
create or replace function notify_msg_sent() returns trigger as $$
begin
    if TG_TABLE_NAME = 'msg' then
        if OLD.time_sent is null and NEW.time_sent is not null then
            perform pg_notify('new_msg_to', NEW.id::text || ',' || addr)
            from msg_to where msg_id = NEW.id;

            perform pg_notify('new_msg_to', NEW.id::text || ',' || addr)
            from msg_add_to where msg_id = NEW.id;
        end if;
    elsif NEW.time_delivered is null then
        perform pg_notify('new_msg_to', NEW.msg_id::text || ',' || NEW.addr)
        from msg where id = NEW.msg_id and time_sent is not null;
    end if;
    return NEW;
end;
$$ language plpgsql;

drop trigger if exists trg_msg_to_insert on msg_to;
create trigger trg_msg_to_insert
    after insert on msg_to
    for each row execute function notify_msg_sent();

drop trigger if exists trg_msg_add_to_insert on msg_add_to;
create trigger trg_msg_add_to_insert
    after insert on msg_add_to
    for each row execute function notify_msg_sent();

drop trigger if exists trg_msg_sent on msg;
create trigger trg_msg_sent
    after update on msg
    for each row execute function notify_msg_sent();

-- Notify listeners (channel new_msg) that a message has become sent/arrived:
-- time_sent set for the first time, on insert (e.g. a message received from a
-- remote host) or update (a local draft being sent). Unlike new_msg_to this
-- fires regardless of recipient domain, so push-notification listeners can wake
-- without polling. Payload is "<msg id>,<addr>", one notification per recipient
-- -- the listener checks addr against its currently-subscribed clients and only
-- fetches message detail for those that are connected.
--
-- This is a DEFERRABLE constraint trigger so it runs at COMMIT: on insert the
-- msg row is written before its msg_to/msg_add_to rows (FK ordering), so a
-- plain row trigger would see no recipients. At commit every recipient row in
-- the transaction is visible.
create or replace function notify_new_msg() returns trigger as $$
begin
    if (TG_OP = 'INSERT' and NEW.time_sent is not null) or
       (TG_OP = 'UPDATE' and OLD.time_sent is null and NEW.time_sent is not null) then
        perform pg_notify('new_msg', NEW.id::text || ',' || addr)
        from msg_to where msg_id = NEW.id;

        perform pg_notify('new_msg', NEW.id::text || ',' || addr)
        from msg_add_to where msg_id = NEW.id;
    end if;
    return NEW;
end;
$$ language plpgsql;

drop trigger if exists trg_new_msg on msg;
create constraint trigger trg_new_msg
    after insert or update on msg
    deferrable initially deferred
    for each row execute function notify_new_msg();

-- Notify the sender (channel delivered) once a recipient's delivery is
-- confirmed, so the sender's UI can unlock replying without a manual reload.
-- Fires on the NULL -> non-NULL transition of time_delivered, which happens
-- once per recipient row regardless of who performs the UPDATE (fmsgd's own
-- remote delivery, its local-domain delivery, or fmsg-webapi's same-domain
-- delivery) -- triggering on the tables rather than the call site covers all
-- of them. Payload is "<msg id>,<from_addr>", the same shape as new_msg's
-- payload but with the sender's address instead of the recipient's, since
-- it's the sender whose UI needs to react. Unlike trg_new_msg this does not
-- need to be deferred: the msg row referenced by msg_id already exists (FK)
-- by the time msg_to/msg_add_to is updated.
create or replace function notify_delivered() returns trigger as $$
begin
    perform pg_notify('delivered', NEW.msg_id::text || ',' || m.from_addr)
    from msg m where m.id = NEW.msg_id;
    return NEW;
end;
$$ language plpgsql;

drop trigger if exists trg_msg_to_delivered on msg_to;
create trigger trg_msg_to_delivered
    after update of time_delivered on msg_to
    for each row
    when (OLD.time_delivered is null and NEW.time_delivered is not null)
    execute function notify_delivered();

drop trigger if exists trg_msg_add_to_delivered on msg_add_to;
create trigger trg_msg_add_to_delivered
    after update of time_delivered on msg_add_to
    for each row
    when (OLD.time_delivered is null and NEW.time_delivered is not null)
    execute function notify_delivered();

-- Wake the sender's outgoing worker (channel new_msg_to) for a pending
-- participant notification, mirroring notify_msg_sent for recipient rows.
-- The payload is advisory only: the worker re-polls fully on any wake-up.
create or replace function notify_add_to_notify_pending() returns trigger as $$
begin
    perform pg_notify('new_msg_to', b.msg_id::text || ',' || NEW.domain)
    from msg_add_to_batch b
    inner join msg m on m.id = b.msg_id
    where b.id = NEW.batch_id and m.time_sent is not null;
    return NEW;
end;
$$ language plpgsql;

drop trigger if exists trg_msg_add_to_notify_insert on msg_add_to_notify;
create trigger trg_msg_add_to_notify_insert
    after insert on msg_add_to_notify
    for each row execute function notify_add_to_notify_pending();

-- Notify listeners (channel recipients_added) that an add-to batch was
-- recorded against a sent message, so existing participants' clients learn of
-- the new recipients without polling. Fires wherever a batch is recorded --
-- added locally through the Web API or received from a remote host -- because
-- both paths insert a msg_add_to_batch row. Payload is "<msg id>,<addr>", one
-- notification per participant (from, every msg_to and every msg_add_to
-- address, including the new batch's own recipients, who have no other
-- realtime event for a message that was sent before they were added); the
-- listener checks addr against its currently-connected clients, exactly as
-- new_msg. Like trg_new_msg this is a deferred constraint trigger: the
-- batch's own msg_add_to rows are inserted after the batch row, so only at
-- commit is the full recipient set visible.
create or replace function notify_recipients_added() returns trigger as $$
begin
    if not exists (select 1 from msg where id = NEW.msg_id and time_sent is not null) then
        return NEW;
    end if;

    perform pg_notify('recipients_added', NEW.msg_id::text || ',' || from_addr)
    from msg where id = NEW.msg_id;

    perform pg_notify('recipients_added', NEW.msg_id::text || ',' || addr)
    from msg_to where msg_id = NEW.msg_id;

    perform pg_notify('recipients_added', NEW.msg_id::text || ',' || addr)
    from msg_add_to where msg_id = NEW.msg_id;

    return NEW;
end;
$$ language plpgsql;

drop trigger if exists trg_recipients_added on msg_add_to_batch;
create constraint trigger trg_recipients_added
    after insert on msg_add_to_batch
    deferrable initially deferred
    for each row execute function notify_recipients_added();

-- Sent protocol fields are immutable. Relational pid links and delivery/read
-- metadata remain bookkeeping and may change.
create or replace function protect_msg_identity() returns trigger as $$
begin
    if OLD.time_sent is not null and
       row(NEW.time_sent,NEW.sha256,NEW.version,NEW.psha256,NEW.no_reply,
           NEW.is_important,NEW.is_terminal,NEW.is_deflate,NEW.from_addr,
           NEW.topic,NEW.type,NEW.size,NEW.filepath,NEW.wire_header,NEW.wire_message)
       is distinct from
       row(OLD.time_sent,OLD.sha256,OLD.version,OLD.psha256,OLD.no_reply,
           OLD.is_important,OLD.is_terminal,OLD.is_deflate,OLD.from_addr,
           OLD.topic,OLD.type,OLD.size,OLD.filepath,OLD.wire_header,OLD.wire_message) then
        raise exception 'sent message content, timestamp and hash are immutable';
    end if;
    return NEW;
end;
$$ language plpgsql;
drop trigger if exists trg_msg_identity on msg;
create trigger trg_msg_identity before update on msg for each row execute function protect_msg_identity();

-- Validate after all rows in the transaction have been assembled. An original
-- first received via add-to has its payload representation on the received batch.
create or replace function require_sent_msg_hash() returns trigger as $$
begin
    if exists (select 1 from msg m where m.id=NEW.id and m.time_sent is not null
               and (m.sha256 is null or octet_length(m.sha256) <> 32 or
                    (m.wire_message is null and not exists (
                        select 1 from msg_add_to_batch b where b.msg_id=m.id
                        and b.sha256 is not null and b.wire_message is not null)))) then
        raise exception 'sent message % requires a 32-byte sha256 and a wire representation', NEW.id;
    end if;
    if exists (select 1 from msg_add_to_batch b join msg m on m.id=b.msg_id
               where m.id=NEW.id and m.time_sent is not null
               and (b.sha256 is null or octet_length(b.sha256) <> 32 or b.wire_message is null)) then
        raise exception 'sent message % has an unfinalized add-to batch', NEW.id;
    end if;
    return null;
end;
$$ language plpgsql;
drop trigger if exists trg_msg_require_hash on msg;
create constraint trigger trg_msg_require_hash after insert or update on msg
    deferrable initially deferred for each row execute function require_sent_msg_hash();

create or replace function require_sent_batch_hash() returns trigger as $$
begin
    if exists (select 1 from msg_add_to_batch b join msg m on m.id=b.msg_id
               where b.id=NEW.id and m.time_sent is not null
               and (b.sha256 is null or octet_length(b.sha256) <> 32 or b.wire_message is null)) then
        raise exception 'sent batch % requires a 32-byte sha256 and a wire representation', NEW.id;
    end if;
    return null;
end;
$$ language plpgsql;
drop trigger if exists trg_batch_require_hash on msg_add_to_batch;
create constraint trigger trg_batch_require_hash after insert or update on msg_add_to_batch
    deferrable initially deferred for each row execute function require_sent_batch_hash();

create or replace function protect_msg_parts() returns trigger as $$
declare
    message_id bigint;
    frozen boolean;
begin
    -- Delivery and read receipts do not change a protocol recipient.
    if TG_OP='UPDATE' then
        if TG_TABLE_NAME='msg_to' then
            if row(NEW.id,NEW.msg_id,NEW.addr) is not distinct from row(OLD.id,OLD.msg_id,OLD.addr) then return NEW; end if;
        end if;
        if TG_TABLE_NAME='msg_add_to' then
            if row(NEW.id,NEW.msg_id,NEW.batch_id,NEW.addr) is not distinct from row(OLD.id,OLD.msg_id,OLD.batch_id,OLD.addr) then return NEW; end if;
        end if;
    end if;
    if TG_OP='DELETE' then message_id=OLD.msg_id; else message_id=NEW.msg_id; end if;
    if TG_OP='UPDATE' and NEW.msg_id <> OLD.msg_id then raise exception 'cannot move message parts'; end if;
    if TG_TABLE_NAME='msg_add_to' then
        if TG_OP='DELETE' then
            select sha256 is not null into frozen from msg_add_to_batch where id=OLD.batch_id for update;
        else
            if TG_OP='UPDATE' and NEW.batch_id <> OLD.batch_id then raise exception 'cannot move batch recipients'; end if;
            select sha256 is not null into frozen from msg_add_to_batch where id=NEW.batch_id and msg_id=message_id for update;
            if not found then raise exception 'batch does not belong to message'; end if;
        end if;
    else
        select time_sent is not null into frozen from msg where id=message_id for update;
    end if;
    if frozen then raise exception 'finalized message parts are immutable'; end if;
    if TG_OP='DELETE' then return OLD; end if;
    return NEW;
end;
$$ language plpgsql;
-- AFTER INSERT allows an ON CONFLICT DO NOTHING receipt to remain a no-op.
drop trigger if exists trg_msg_to_content on msg_to;
create trigger trg_msg_to_content after insert or update or delete on msg_to for each row execute function protect_msg_parts();
drop trigger if exists trg_msg_attachment_content on msg_attachment;
create trigger trg_msg_attachment_content after insert or update or delete on msg_attachment for each row execute function protect_msg_parts();
drop trigger if exists trg_msg_add_to_content on msg_add_to;
create trigger trg_msg_add_to_content after insert or update or delete on msg_add_to for each row execute function protect_msg_parts();

create or replace function protect_batch_identity() returns trigger as $$
begin
    if OLD.sha256 is not null and
       row(NEW.msg_id,NEW.add_to_from,NEW.time_added,NEW.sha256,NEW.wire_message)
       is distinct from row(OLD.msg_id,OLD.add_to_from,OLD.time_added,OLD.sha256,OLD.wire_message) then
        raise exception 'finalized add-to batch is immutable';
    end if;
    return NEW;
end;
$$ language plpgsql;
drop trigger if exists trg_batch_identity on msg_add_to_batch;
create trigger trg_batch_identity before update on msg_add_to_batch for each row execute function protect_batch_identity();
