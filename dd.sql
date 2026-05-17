/****************************************************************
 *
 * PostgreSQL database objects data definition for fmsgd
 *
 ****************************************************************/

-- database with encoding UTF8 should already be created and connected

create table if not exists msg (
    id            	bigserial       	primary key,
	version			int					not null,
    pid           	bigint          	references msg (id),
	no_reply		boolean				not null default false,
	is_important	boolean				not null default false,
	is_deflate		boolean				not null default false,
    time_sent     	double precision,             -- time sending host recieved message for sending, message timestamp field, NULL means message not ready for sending i.e. draft
    from_addr     	varchar(255)    	not null,
    add_to_from   	varchar(255),
    topic         	varchar(255)    	not null, 
    type          	varchar(255)    	not null,
    sha256        	bytea           	unique,
    psha256       	bytea,
	size			int					not null, -- spec allows uint32 but we don't enforced by FMSG_MAX_MSG_SIZE
    filepath      	text            	not null
);
create index on msg ((lower(from_addr)));

create table if not exists msg_to (
	id				bigserial			primary key,
	msg_id			bigint				not null references msg (id),
	addr			varchar(255)		not null,
    time_delivered  double precision,   -- if sending, time sending host recieved delivery confirmation, if receiving, time successfully received message
    time_last_attempt double precision, -- only used when sending, time of last delivery attempt if failed; otherwise null
    time_read       double precision,   -- time recipient read the message; null if unread
    response_code   smallint,		    -- only used when sending, response code of last delivery attempt if failed; otherwise null
    attempt_count   int             not null default 0, -- number of failed delivery attempts; used for exponential back-off
	unique (msg_id, addr)
);
create index on msg_to ((lower(addr)));

create table if not exists msg_add_to (
	id				bigserial			primary key,
	msg_id			bigint				not null references msg (id),
	addr			varchar(255)		not null,
    time_delivered  double precision,   -- if sending, time sending host recieved delivery confirmation, if receiving, time successfully received message
    time_last_attempt double precision, -- only used when sending, time of last delivery attempt if failed; otherwise null
    time_read       double precision,   -- time recipient read the message; null if unread
    response_code   smallint,		    -- only used when sending, response code of last delivery attempt if failed; otherwise null
    attempt_count   int             not null default 0, -- number of failed delivery attempts; used for exponential back-off
	unique (msg_id, addr)
);
create index on msg_add_to ((lower(addr)));

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

-- keep protocol parent hash populated for locally-created replies that set
-- the relational parent id. A reply cannot reference a draft parent, and any
-- explicit psha256 must match the referenced parent's sha256.
create or replace function populate_msg_psha256_from_pid() returns trigger as $$
declare
    parent_time_sent double precision;
    parent_sha256 bytea;
begin
    if NEW.pid is null then
        return NEW;
    end if;

    select parent.time_sent, parent.sha256
    into parent_time_sent, parent_sha256
    from msg parent
    where parent.id = NEW.pid;

    if not found then
        raise exception 'parent message % does not exist', NEW.pid;
    end if;

    if parent_time_sent is null then
        raise exception 'cannot set pid %: parent message is a draft', NEW.pid;
    end if;

    if parent_sha256 is null or octet_length(parent_sha256) = 0 then
        -- parent was delivered locally only and has no sha256 yet; psha256 cannot be populated
        return NEW;
    end if;

    if NEW.psha256 is null or octet_length(NEW.psha256) = 0 then
        NEW.psha256 = parent_sha256;
    elsif NEW.psha256 <> parent_sha256 then
        raise exception 'psha256 does not match parent message % sha256', NEW.pid;
    end if;

    return NEW;
end;
$$ language plpgsql;

drop trigger if exists trg_msg_populate_psha256 on msg;
create trigger trg_msg_populate_psha256
    before insert or update of pid, psha256 on msg
    for each row execute function populate_msg_psha256_from_pid();

-- once a message has replies, it must remain referenceable by protocol hash.
create or replace function prevent_referenced_msg_from_becoming_unreferenceable() returns trigger as $$
begin
    if exists (select 1 from msg child where child.pid = NEW.id) then
        if NEW.time_sent is null then
            raise exception 'cannot make message % a draft: it has replies', NEW.id;
        end if;

        if OLD.sha256 is not null and (NEW.sha256 is null or octet_length(NEW.sha256) = 0) then
            raise exception 'cannot clear sha256 for message %: it has replies', NEW.id;
        end if;

        if OLD.sha256 is distinct from NEW.sha256 then
            raise exception 'cannot change sha256 for message %: it has replies', NEW.id;
        end if;
    end if;
    return NEW;
end;
$$ language plpgsql;

drop trigger if exists trg_msg_prevent_unreferenceable_parent on msg;
create trigger trg_msg_prevent_unreferenceable_parent
    before update of time_sent, sha256 on msg
    for each row execute function prevent_referenced_msg_from_becoming_unreferenceable();

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
