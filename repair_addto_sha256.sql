/****************************************************************
 *
 * One-off repair: canonical sha256 for received add-to messages
 *
 ****************************************************************
 *
 * Before the "canonical add-to hash" fix, storeMsgDetail /
 * storeMsgHeaderOnly stored the add-to *variant* message hash in
 * msg.sha256 for messages received via add-to. The canonical
 * (original-form) hash -- the one replies and remote hosts use to
 * reference the message -- was instead written to msg.psha256.
 *
 * Consequently a reply to an add-to message carried an unknown pid
 * and was rejected by the parent's home host with response code 6
 * ("parent not found").
 *
 * This script, for each affected message:
 *   1. moves the canonical hash from psha256 into sha256
 *   2. clears psha256 (an add-to row has no relational parent ref)
 *   3. re-derives psha256 for replies to it, and clears the stale
 *      self-hash of any reply still awaiting delivery so the sender
 *      recomputes it with the corrected wire pid.
 *
 * ---------------------------------------------------------------
 * STEP 1 -- review affected rows, then list their ids in STEP 2.
 *
 *   SELECT m.id, m.from_addr, m.add_to_from,
 *          encode(m.sha256,'hex')  AS variant_sha256,
 *          encode(m.psha256,'hex') AS canonical_sha256,
 *          (SELECT count(*) FROM msg c WHERE c.pid = m.id) AS reply_count
 *   FROM msg m
 *   WHERE EXISTS (SELECT 1 FROM msg_add_to a WHERE a.msg_id = m.id)
 *     AND m.psha256 IS NOT NULL
 *     AND m.sha256 IS DISTINCT FROM m.psha256;
 *
 * Every id you carry into STEP 2 must be a message RECEIVED via
 * add-to. A message composed locally that is itself a reply also
 * has psha256 <> sha256 (psha256 = its real parent, sha256 already
 * correct) -- do NOT include those.
 ****************************************************************/

begin;

-- The triggers that protect referenced messages and auto-populate
-- psha256 would block changing sha256 on a message with replies and
-- interfere with the manual re-derivation below; bypass them here.
set local session_replication_role = replica;

with affected as (
    -- STEP 2: replace the empty array with the confirmed ids.
    select unnest(array[]::bigint[]) as id
),
fixed as (
    update msg m
       set sha256  = m.psha256,
           psha256 = null
      from affected a
     where m.id = a.id
       and m.psha256 is not null
       and exists (select 1 from msg_add_to x where x.msg_id = m.id)
    returning m.id
)
update msg child
   set psha256 = parent.sha256,
       sha256  = case
                   when exists (select 1 from msg_to t
                                 where t.msg_id = child.id
                                   and t.time_delivered is null)
                     then null            -- undelivered: force recompute
                   else child.sha256      -- already delivered: leave as-is
                 end
  from msg parent
 where child.pid = parent.id
   and parent.id in (select id from fixed);

reset session_replication_role;

-- Inspect the result, then COMMIT to apply or ROLLBACK to abort.
-- commit;
rollback;
