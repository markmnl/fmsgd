-- Test script: insert a message into msg + msg_to for sender to pick up.
--
-- Before running:
--   1. Create the message body file at the filepath below, e.g.:
--        echo Hello, this is a test message> C:\Data\fmsg.io\fmsg.io\mark\out\test.txt
--   2. Adjust from_addr, to addr, and filepath to match your setup.
--   3. The sha256 must be a unique value per message; this uses a random one.

DO $$
DECLARE
    v_msg_id bigint;
    v_now    double precision := extract(epoch from now());
    v_sha256 bytea            := repeat(E'\\000', 32)::bytea;
BEGIN
    INSERT INTO msg (
        version,
        flags,
        time_sent,
        from_addr,
        topic,
        type,
        sha256,
        psha256,
        size,
        filepath
    ) VALUES (
        1,                                          -- version
        0,                                          -- flags (no special flags)
        v_now,                                      -- time_sent (current timestamp)
        '@markmnl@hairpin.local',                   -- from_addr (used by receiver to challenge)
        'Test Topic',                               -- topic
        'text/plain',                               -- type
        v_sha256,                                   -- sha256
        NULL,                                       -- psha256 (NULL = no parent)
        44,                                         -- size in bytes (must match file size)
        'C:/Data/fmsg/data/hairpin.local/markmnl/out/20260215_135023/test.txt' -- filepath to message body
    )
    RETURNING id INTO v_msg_id;

    -- recipient on a remote domain (time_delivered = NULL triggers the sender)
    INSERT INTO msg_to (msg_id, addr, time_delivered, time_last_attempt, response_code)
    VALUES (v_msg_id, '@markmnl@localhost', NULL, NULL, NULL);

    RAISE NOTICE 'Inserted msg id=% with sha256=%', v_msg_id, encode(v_sha256, 'hex');
END $$;
