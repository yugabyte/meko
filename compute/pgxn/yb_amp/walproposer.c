/*-------------------------------------------------------------------------
 *
 * walproposer.c
 *
 * Proposer/leader part of the total order broadcast protocol between postgres
 * and WAL safekeepers.
 *
 * We have two ways of launching WalProposer:
 *
 *   1. As a background worker which will pretend to be physical WalSender.
 * 		WalProposer will receive notifications about new available WAL and
 * 		will immediately broadcast it to alive safekeepers.
 *
 *   2. As a standalone utility by running `postgres --sync-safekeepers`. That
 *      is needed to create LSN from which it is safe to start postgres. More
 *      specifically it addresses following problems:
 *
 *      a) Chicken-or-the-egg problem: compute postgres needs data directory
 *         with non-rel files that are downloaded from pageserver by calling
 *         basebackup@LSN. This LSN is not arbitrary, it must include all
 *         previously committed transactions and defined through consensus
 *         voting, which happens... in walproposer, a part of compute node.
 *
 *      b) Just warranting such LSN is not enough, we must also actually commit
 *         it and make sure there is a safekeeper who knows this LSN is
 *         committed so WAL before it can be streamed to pageserver -- otherwise
 *         basebackup will hang waiting for WAL. Advancing commit_lsn without
 *         playing consensus game is impossible, so speculative 'let's just poll
 *         safekeepers, learn start LSN of future epoch and run basebackup'
 *         won't work.
 *
 * Both ways are implemented in walproposer_pg.c file. This file contains
 * generic part of walproposer which can be used in both cases, but can also
 * be used as an independent library.
 *
 *-------------------------------------------------------------------------
 */
#include <sys/resource.h>

#include "postgres.h"
#include "libpq/pqformat.h"
#include "storage/waiteventset.h"
#include "yb_amp.h"
#include "walproposer.h"
#include "yb_amp_utils.h"

/* Prototypes for private functions */
static void WalProposerLoop(WalProposer *wp);
static void ShutdownConnection(Safekeeper *sk);
static void ResetConnection(Safekeeper *sk);
static long TimeToReconnect(WalProposer *wp, TimestampTz now);
static void ReconnectSafekeepers(WalProposer *wp);
static void AdvancePollState(Safekeeper *sk, uint32 events);
static void HandleConnectionEvent(Safekeeper *sk);
static void SendStartWALPush(Safekeeper *sk);
static void RecvStartWALPushResult(Safekeeper *sk);
static void SendProposerGreeting(Safekeeper *sk);
static void RecvAcceptorGreeting(Safekeeper *sk);
static void HandleElectedProposer(WalProposer *wp);
static void ProcessPropStartPos(WalProposer *wp);
static void SendProposerElected(Safekeeper *sk);
static void StartStreaming(Safekeeper *sk);
static void SendMessageToNode(Safekeeper *sk);
static void BroadcastAppendRequest(WalProposer *wp);
static void HandleActiveState(Safekeeper *sk, uint32 events);
static bool SendAppendRequests(Safekeeper *sk);
static bool RecvAppendResponses(Safekeeper *sk);
static XLogRecPtr GetAcknowledgedByQuorumWALPosition(WalProposer *wp);
static void PAMessageSerialize(WalProposer *wp, ProposerAcceptorMessage *msg, StringInfo buf, int proto_version);
static void HandleSafekeeperResponse(WalProposer *wp, Safekeeper *sk);
static bool AsyncRead(Safekeeper *sk, char **buf, int *buf_size);
static bool AsyncReadMessage(Safekeeper *sk, AcceptorProposerMessage *anymsg);
static bool BlockingWrite(Safekeeper *sk, void *msg, size_t msg_size, SafekeeperState success_state);
static bool AsyncWrite(Safekeeper *sk, void *msg, size_t msg_size, SafekeeperState flush_state);
static bool AsyncFlush(Safekeeper *sk);
static char *FormatSafekeeperState(Safekeeper *sk);
static void AssertEventsOkForState(uint32 events, Safekeeper *sk);
static char *FormatEvents(WalProposer *wp, uint32 events);
static void UpdateDonorShmem(WalProposer *wp);

WalProposer *
WalProposerCreate(WalProposerConfig *config, walproposer_api api)
{
	char	   *host;
	char	   *sep;
	char	   *port;
	WalProposer *wp;

	wp = palloc0(sizeof(WalProposer));
	wp->config = config;
	wp->api = api;
	wp->localTimeLineID = config->pgTimeline;
	wp->state = WPS_COLLECTING_TERMS;

	wp_log(LOG, "yb_amp.safekeepers=%s", wp->config->safekeepers_list);

	/*
	 * Membership/generations were removed in yb-amp (single safekeeper per
	 * branch), so the list is a plain comma-separated set of host:port pairs.
	 */
	host = wp->config->safekeepers_list;

	for (; host != NULL && *host != '\0'; host = sep)
	{
		port = strchr(host, ':');
		if (port == NULL)
		{
			wp_log(FATAL, "port is not specified");
		}
		*port++ = '\0';
		sep = strchr(port, ',');
		if (sep != NULL)
			*sep++ = '\0';
		if (wp->n_safekeepers + 1 >= MAX_SAFEKEEPERS)
		{
			wp_log(FATAL, "too many safekeepers");
		}
		wp->safekeeper[wp->n_safekeepers].host = host;
		wp->safekeeper[wp->n_safekeepers].port = port;
		wp->safekeeper[wp->n_safekeepers].state = SS_OFFLINE;
		wp->safekeeper[wp->n_safekeepers].active_state = SS_ACTIVE_SEND;
		wp->safekeeper[wp->n_safekeepers].wp = wp;
		/* BEGIN_HADRON */
		wp->safekeeper[wp->n_safekeepers].index = wp->n_safekeepers;
		/* END_HADRON */
		{
			Safekeeper *sk = &wp->safekeeper[wp->n_safekeepers];
			int			written = 0;

			written = snprintf((char *) &sk->conninfo, MAXCONNINFO,
							   "%s host=%s port=%s dbname=replication options='-c branch_id=%s'",
							   wp->config->safekeeper_conninfo_options, sk->host, sk->port,
							   wp->config->branch_id);
			if (written > MAXCONNINFO || written < 0)
				wp_log(FATAL, "could not create connection string for safekeeper %s:%s", sk->host, sk->port);
		}

		initStringInfo(&wp->safekeeper[wp->n_safekeepers].outbuf);
		wp->safekeeper[wp->n_safekeepers].startStreamingAt = InvalidXLogRecPtr;
		wp->safekeeper[wp->n_safekeepers].streamingAt = InvalidXLogRecPtr;
		wp->n_safekeepers += 1;
	}
	if (wp->n_safekeepers < 1)
	{
		wp_log(FATAL, "safekeepers addresses are not specified");
	}

	if (wp->config->proto_version != 3)
		wp_log(FATAL, "unsupported safekeeper protocol version %d; only v3 is supported",
			   wp->config->proto_version);
	wp_log(LOG, "using safekeeper protocol version %d", wp->config->proto_version);
	
	/* BEGIN_HADRON */
	wp->api.reset_safekeeper_statuses_for_metrics(wp, wp->n_safekeepers);
	/* END_HADRON */

	/* Fill the greeting package */
	wp->greetRequest.pam.tag = 'g';
	if (!wp->config->branch_id)
		wp_log(FATAL, "yb_amp.branch_id is not provided");
	wp->greetRequest.branch_id = wp->config->branch_id;
	wp->greetRequest.pg_version = PG_VERSION_NUM;
	wp->greetRequest.system_id = wp->config->systemId;
	wp->greetRequest.wal_seg_size = wp->config->wal_segment_size;

	wp->api.init_event_set(wp);

	return wp;
}

void
WalProposerFree(WalProposer *wp)
{
	for (int i = 0; i < wp->n_safekeepers; i++)
	{
		Safekeeper *sk = &wp->safekeeper[i];

		Assert(sk->outbuf.data != NULL);
		pfree(sk->outbuf.data);
	}

	pfree(wp);
}

/*
 * Create new AppendRequest message and start sending it. This function is
 * called from walsender every time the new WAL is available.
 */
void
WalProposerBroadcast(WalProposer *wp, XLogRecPtr startpos, XLogRecPtr endpos)
{
	Assert(startpos == wp->availableLsn && endpos >= wp->availableLsn);
	wp->availableLsn = endpos;
	BroadcastAppendRequest(wp);
}

/*
 * Advance the WAL proposer state machine, waiting each time for events to occur.
 * Will exit only when latch is set, i.e. new WAL should be pushed from walsender
 * to walproposer.
 */
void
WalProposerPoll(WalProposer *wp)
{
	while (true)
	{
		Safekeeper *sk = NULL;
		int			rc = 0;
		uint32		events = 0;
		TimestampTz now = wp->api.get_current_timestamp(wp);
		long		timeout = TimeToReconnect(wp, now);

		rc = wp->api.wait_event_set(wp, timeout, &sk, &events);

		/* Exit loop if latch is set (we got new WAL) */
		if (rc == 1 && (events & WL_LATCH_SET))
			break;

		/*
		 * If the event contains something that one of our safekeeper states
		 * was waiting for, we'll advance its state.
		 */
		if (rc == 1 && (events & WL_SOCKET_MASK))
		{
			Assert(sk != NULL);
			AdvancePollState(sk, events);
		}

		/*
		 * If the timeout expired, attempt to reconnect to any safekeepers
		 * that we dropped
		 */
		ReconnectSafekeepers(wp);

		if (rc == 0)			/* timeout expired */
		{
			/*
			 * Ensure flushrecptr is set to a recent value. This fixes a case
			 * where we've not been notified of new WAL records when we were
			 * planning on consuming them.
			 */
			if (!wp->config->syncSafekeepers)
			{
				XLogRecPtr	flushed = wp->api.get_flush_rec_ptr(wp);

				if (flushed > wp->availableLsn)
					break;
			}
		}

		now = wp->api.get_current_timestamp(wp);
		/* timeout expired: poll state */
		if (rc == 0 || TimeToReconnect(wp, now) <= 0)
		{
			/*
			 * If no WAL was generated during timeout (and we have already
			 * collected the quorum), then send empty keepalive message
			 */
			if (wp->availableLsn != InvalidXLogRecPtr)
			{
				BroadcastAppendRequest(wp);
			}

			/*
			 * Abandon connection attempts which take too long.
			 */
			now = wp->api.get_current_timestamp(wp);
			for (int i = 0; i < wp->n_safekeepers; i++)
			{
				sk = &wp->safekeeper[i];
				if (TimestampDifferenceExceeds(sk->latestMsgReceivedAt, now,
											   wp->config->safekeeper_connection_timeout))
				{
					wp_log(WARNING, "terminating connection to safekeeper '%s:%s' in '%s' state: no messages received during the last %dms or connection attempt took longer than that",
						   sk->host, sk->port, FormatSafekeeperState(sk), wp->config->safekeeper_connection_timeout);
					ShutdownConnection(sk);
				}
			}
		}
	}
}

void
WalProposerStart(WalProposer *wp)
{

	/* Initiate connections to all safekeeper nodes */
	for (int i = 0; i < wp->n_safekeepers; i++)
	{
		ResetConnection(&wp->safekeeper[i]);
	}

	WalProposerLoop(wp);
}

static void
WalProposerLoop(WalProposer *wp)
{
	while (true)
		WalProposerPoll(wp);
}


/* Shuts down and cleans up the connection for a safekeeper. Sets its state to SS_OFFLINE */
static void
ShutdownConnection(Safekeeper *sk)
{
	sk->state = SS_OFFLINE;
	sk->streamingAt = InvalidXLogRecPtr;

	/* BEGIN_HADRON */
	sk->wp->api.update_safekeeper_status_for_metrics(sk->wp, sk->index, 0);
	/* END_HADRON */

	sk->wp->api.conn_finish(sk);
	sk->wp->api.rm_safekeeper_event_set(sk);
}

/*
 * This function is called to establish new connection or to reestablish
 * connection in case of connection failure.
 *
 * On success, sets the state to SS_CONNECTING_WRITE.
 */
static void
ResetConnection(Safekeeper *sk)
{
	WalProposer *wp = sk->wp;

	if (sk->state != SS_OFFLINE)
	{
		ShutdownConnection(sk);
	}

	/*
	 * Try to establish new connection, it will update sk->conn.
	 */
	wp->api.conn_connect_start(sk);

	/*
	 * PQconnectStart won't actually start connecting until we run
	 * PQconnectPoll. Before we do that though, we need to check that it
	 * didn't immediately fail.
	 */
	if (wp->api.conn_status(sk) == WP_CONNECTION_BAD)
	{
		/*---
		 * According to libpq docs:
		 *   "If the result is CONNECTION_BAD, the connection attempt has already failed,
		 *    typically because of invalid connection parameters."
		 * We should report this failure. Do not print the exact `conninfo` as it may
		 * contain e.g. password. The error message should already provide enough information.
		 *
		 * https://www.postgresql.org/docs/devel/libpq-connect.html#LIBPQ-PQCONNECTSTARTPARAMS
		 */
		wp_log(WARNING, "immediate failure to connect with node '%s:%s':\n\terror: %s",
			   sk->host, sk->port, wp->api.conn_error_message(sk));

		/*
		 * Even though the connection failed, we still need to clean up the
		 * object
		 */
		wp->api.conn_finish(sk);
		return;
	}

	/*
	 * The documentation for PQconnectStart states that we should call
	 * PQconnectPoll in a loop until it returns PGRES_POLLING_OK or
	 * PGRES_POLLING_FAILED. The other two possible returns indicate whether
	 * we should wait for reading or writing on the socket. For the first
	 * iteration of the loop, we're expected to wait until the socket becomes
	 * writable.
	 *
	 * The wording of the documentation is a little ambiguous; thankfully
	 * there's an example in the postgres source itself showing this behavior.
	 * (see libpqrcv_connect, defined in
	 * src/backend/replication/libpqwalreceiver/libpqwalreceiver.c)
	 */
	wp_log(LOG, "connecting with node %s:%s", sk->host, sk->port);

	sk->state = SS_CONNECTING_WRITE;
	sk->latestMsgReceivedAt = wp->api.get_current_timestamp(wp);

	wp->api.add_safekeeper_event_set(sk, WL_SOCKET_WRITEABLE);
	return;
}

/*
 * How much milliseconds left till we should attempt reconnection to
 * safekeepers? Returns 0 if it is already high time, -1 if we never reconnect
 * (do we actually need this?).
 */
static long
TimeToReconnect(WalProposer *wp, TimestampTz now)
{
	TimestampTz passed;
	TimestampTz till_reconnect;

	if (wp->config->safekeeper_reconnect_timeout <= 0)
		return -1;

	passed = now - wp->last_reconnect_attempt;
	till_reconnect = wp->config->safekeeper_reconnect_timeout * 1000 - passed;
	if (till_reconnect <= 0)
		return 0;
	return (long) (till_reconnect / 1000);
}

/* If the timeout has expired, attempt to reconnect to all offline safekeepers */
static void
ReconnectSafekeepers(WalProposer *wp)
{
	TimestampTz now = wp->api.get_current_timestamp(wp);

	if (TimeToReconnect(wp, now) == 0)
	{
		wp->last_reconnect_attempt = now;
		for (int i = 0; i < wp->n_safekeepers; i++)
		{
			if (wp->safekeeper[i].state == SS_OFFLINE)
				ResetConnection(&wp->safekeeper[i]);
		}
	}
}

/*
 * Performs the logic for advancing the state machine of the specified safekeeper,
 * given that a certain set of events has occurred.
 */
static void
AdvancePollState(Safekeeper *sk, uint32 events)
{
#ifdef WALPROPOSER_LIB			/* wp_log needs wp in lib build */
	WalProposer *wp = sk->wp;
#endif

	/*
	 * Sanity check. We assume further down that the operations don't block
	 * because the socket is ready.
	 */
	AssertEventsOkForState(events, sk);

	/* Execute the code corresponding to the current state */
	switch (sk->state)
	{
			/*
			 * safekeepers are only taken out of SS_OFFLINE by calls to
			 * ResetConnection
			 */
		case SS_OFFLINE:
			wp_log(FATAL, "unexpected safekeeper %s:%s state advancement: is offline",
				   sk->host, sk->port);
			break;				/* actually unreachable, but prevents
								 * -Wimplicit-fallthrough */

			/*
			 * Both connecting states run the same logic. The only difference
			 * is the events they're expecting
			 */
		case SS_CONNECTING_READ:
		case SS_CONNECTING_WRITE:
			HandleConnectionEvent(sk);
			break;

			/*
			 * Waiting for a successful CopyBoth response.
			 */
		case SS_WAIT_EXEC_RESULT:
			RecvStartWALPushResult(sk);
			break;

			/*
			 * Finish handshake comms: receive the safekeeper's greeting. In
			 * yb-amp this also elects us and sends ProposerElected (there is no
			 * voting), transitioning straight to SS_SEND_ELECTED_FLUSH.
			 */
		case SS_HANDSHAKE_RECV:
			RecvAcceptorGreeting(sk);
			break;

			/* Flush proposer announcement message */
		case SS_SEND_ELECTED_FLUSH:

			/*
			 * AsyncFlush ensures we only move on to SS_ACTIVE once the flush
			 * completes. If we still have more to do, we'll wait until the
			 * next poll comes along.
			 */
			if (!AsyncFlush(sk))
				return;

			/* flush is done, event set and state will be updated later */
			StartStreaming(sk);
			break;

			/*
			 * Active state is used for streaming WAL and receiving feedback.
			 */
		case SS_ACTIVE:
			HandleActiveState(sk, events);
			break;
	}
}

static void
HandleConnectionEvent(Safekeeper *sk)
{
	WalProposer *wp = sk->wp;
	WalProposerConnectPollStatusType result = wp->api.conn_connect_poll(sk);

	/* The new set of events we'll wait on, after updating */
	uint32		new_events = WL_NO_EVENTS;

	switch (result)
	{
		case WP_CONN_POLLING_OK:
			wp_log(LOG, "connected with node %s:%s", sk->host,
				   sk->port);
			sk->latestMsgReceivedAt = wp->api.get_current_timestamp(wp);

			/*
			 * We have to pick some event to update event set. We'll
			 * eventually need the socket to be readable, so we go with that.
			 */
			new_events = WL_SOCKET_READABLE;
			break;

			/*
			 * If we need to poll to finish connecting, continue doing that
			 */
		case WP_CONN_POLLING_READING:
			sk->state = SS_CONNECTING_READ;
			new_events = WL_SOCKET_READABLE;
			break;
		case WP_CONN_POLLING_WRITING:
			sk->state = SS_CONNECTING_WRITE;
			new_events = WL_SOCKET_WRITEABLE;
			break;

		case WP_CONN_POLLING_FAILED:
			wp_log(WARNING, "failed to connect to node '%s:%s': %s",
				   sk->host, sk->port, wp->api.conn_error_message(sk));

			/*
			 * If connecting failed, we don't want to restart the connection
			 * because that might run us into a loop. Instead, shut it down --
			 * it'll naturally restart at a slower interval on calls to
			 * ReconnectSafekeepers.
			 */
			ShutdownConnection(sk);
			return;
	}

	/*
	 * Because PQconnectPoll can change the socket, we have to un-register the
	 * old event and re-register an event on the new socket.
	 */
	wp->api.rm_safekeeper_event_set(sk);
	wp->api.add_safekeeper_event_set(sk, new_events);

	/* If we successfully connected, send START_WAL_PUSH query */
	if (result == WP_CONN_POLLING_OK)
		SendStartWALPush(sk);
}

/*
 * Send "START_WAL_PUSH" message as an empty query to the safekeeper. Performs
 * a blocking send, then immediately moves to SS_WAIT_EXEC_RESULT. If something
 * goes wrong, change state to SS_OFFLINE and shutdown the connection.
 */
static void
SendStartWALPush(Safekeeper *sk)
{
	WalProposer *wp = sk->wp;

	/*
	 * yb-amp allows implicit timeline creation via the walproposer handshake
	 * (membership/generations were removed).
	 */
	char	   *allow_timeline_creation = "true";
#define CMD_LEN 512
	char		cmd[CMD_LEN];


	snprintf(cmd, CMD_LEN, "START_WAL_PUSH (proto_version '%d', allow_timeline_creation '%s')", wp->config->proto_version, allow_timeline_creation);
	if (!wp->api.conn_send_query(sk, cmd))
	{
		wp_log(WARNING, "failed to send '%s' query to safekeeper %s:%s: %s",
			   cmd, sk->host, sk->port, wp->api.conn_error_message(sk));
		ShutdownConnection(sk);
		return;
	}
	sk->state = SS_WAIT_EXEC_RESULT;
	wp->api.update_event_set(sk, WL_SOCKET_READABLE);
}

static void
RecvStartWALPushResult(Safekeeper *sk)
{
	WalProposer *wp = sk->wp;

	switch (wp->api.conn_get_query_result(sk))
	{
			/*
			 * Successful result, move on to starting the handshake
			 */
		case WP_EXEC_SUCCESS_COPYBOTH:

			SendProposerGreeting(sk);
			break;

			/*
			 * Needs repeated calls to finish. Wait until the socket is
			 * readable
			 */
		case WP_EXEC_NEEDS_INPUT:

			/*
			 * SS_WAIT_EXEC_RESULT is always reached through an event, so we
			 * don't need to update the event set
			 */
			break;

		case WP_EXEC_FAILED:
			wp_log(WARNING, "failed to send query to safekeeper %s:%s: %s",
				   sk->host, sk->port, wp->api.conn_error_message(sk));
			ShutdownConnection(sk);
			return;

			/*
			 * Unexpected result -- funamdentally an error, but we want to
			 * produce a custom message, rather than a generic "something went
			 * wrong"
			 */
		case WP_EXEC_UNEXPECTED_SUCCESS:
			wp_log(WARNING, "received bad response from safekeeper %s:%s query execution",
				   sk->host, sk->port);
			ShutdownConnection(sk);
			return;
	}
}

/*
 * Start handshake: first of all send information about the
 * walproposer. After sending, we wait on SS_HANDSHAKE_RECV for
 * a response to finish the handshake.
 */
static void
SendProposerGreeting(Safekeeper *sk)
{
	WalProposer *wp = sk->wp;

	wp_log(LOG, "sending ProposerGreeting to safekeeper %s:%s", sk->host, sk->port);

	PAMessageSerialize(wp, (ProposerAcceptorMessage *) &wp->greetRequest,
					   &sk->outbuf, wp->config->proto_version);

	/*
	 * On failure, logging & resetting the connection is handled. We just need
	 * to handle the control flow.
	 */
	BlockingWrite(sk, sk->outbuf.data, sk->outbuf.len, SS_HANDSHAKE_RECV);
}

/*
 * Receive the acceptor greeting from the (single) safekeeper.
 *
 * yb-amp has no elections: the greeting carries the safekeeper's node id and
 * its end-of-WAL (flushLsn). We immediately treat this proposer as elected,
 * resume streaming from that point, and send the ProposerElected message which
 * truncates the safekeeper's WAL to it.
 */
static void
RecvAcceptorGreeting(Safekeeper *sk)
{
	WalProposer *wp = sk->wp;

	/*
	 * If our reading doesn't immediately succeed, any necessary error
	 * handling or state setting is taken care of. We can leave any other work
	 * until later.
	 */
	sk->greetResponse.apm.tag = 'g';
	if (!AsyncReadMessage(sk, (AcceptorProposerMessage *) &sk->greetResponse))
		return;

	wp_log(LOG, "received AcceptorGreeting from safekeeper %s:%s, node_id = %lu, flushLsn=%X/%X",
		   sk->host, sk->port, sk->greetResponse.nodeId,
		   LSN_FORMAT_ARGS(sk->greetResponse.flushLsn));

	++wp->n_connected;

	/*
	 * Single safekeeper: the greeting alone elects us. Resume streaming from
	 * the safekeeper's end-of-WAL (which it truncates to on ProposerElected).
	 */
	wp->donor = sk;
	wp->propTermStartLsn = sk->greetResponse.flushLsn;
	wp->state = WPS_ELECTED;
	HandleElectedProposer(wp);
}

/*
 * Called once the (single) safekeeper has greeted us and we are the elected
 * proposer. Computes the start position and sends ProposerElected, then starts
 * replication from walsender.
 */
static void
HandleElectedProposer(WalProposer *wp)
{
	ProcessPropStartPos(wp);
	Assert(wp->propTermStartLsn != InvalidXLogRecPtr);

	/*
	 * Synchronously download WAL from the safekeeper. We do that only for
	 * logical replication (and switching logical walsenders to
	 * yb_amp_walreader is a todo.)
	 */
	if (!wp->api.recovery_download(wp, wp->donor))
	{
		wp_log(FATAL, "failed to download WAL for logical replicaiton");
	}

	if (wp->config->syncSafekeepers)
	{
		/*
		 * yb-mode: sync-safekeepers is trivially satisfied after election
		 * because there are no peers to sync to.
		 */
		wp->api.finish_sync_safekeepers(wp, wp->propTermStartLsn);
		/* unreachable */
	}

	SendProposerElected(wp->donor);

	wp->api.start_streaming(wp, wp->propTermStartLsn);
	/* Should not return here */
}

/* If LSN points to the page header, skip it */
static XLogRecPtr
SkipXLogPageHeader(WalProposer *wp, XLogRecPtr lsn)
{
	if (XLogSegmentOffset(lsn, wp->config->wal_segment_size) == 0)
	{
		lsn += SizeOfXLogLongPHD;
	}
	else if (lsn % XLOG_BLCKSZ == 0)
	{
		lsn += SizeOfXLogShortPHD;
	}
	return lsn;
}

/*
 * Determine the proposer's starting position and validate it against the
 * basebackup, then push it to shmem.
 *
 * yb-amp: the single safekeeper's flushLsn (reported in the greeting) is the
 * resume point. On bootstrap (flushLsn == 0) start from the basebackup LSN.
 */
static void
ProcessPropStartPos(WalProposer *wp)
{
	WalproposerShmemState *walprop_shared;

	Assert(wp->state == WPS_ELECTED);

	/*
	 * If propTermStartLsn is 0 the safekeeper has no WAL yet: we are
	 * bootstrapping and nothing was committed. Start streaming from the
	 * basebackup LSN then.
	 *
	 * In case of sync-safekeepers just exit: there is nothing to sync.
	 */
	if (wp->propTermStartLsn == InvalidXLogRecPtr)
	{
		if (!wp->config->syncSafekeepers)
		{
			wp->propTermStartLsn = wp->api.get_redo_start_lsn(wp);
			wp_log(LOG, "bumped epochStartLsn to the first record %X/%X", LSN_FORMAT_ARGS(wp->propTermStartLsn));
		}
		else
		{
			wp_log(LOG, "elected with zero propTermStartLsn in sync-safekeepers, exiting");
			wp->api.finish_sync_safekeepers(wp, wp->propTermStartLsn);
		}
	}
	pg_atomic_write_u64(&wp->api.get_shmem_state(wp)->propEpochStartLsn, wp->propTermStartLsn);

	/*
	 * We will be generating WAL since propTermStartLsn, so we should set
	 * availableLsn to mark this LSN as the latest available position.
	 */
	wp->availableLsn = wp->propTermStartLsn;

	wp_log(LOG, "walproposer elected, epochStartLsn %X/%X, donor %s:%s",
		   LSN_FORMAT_ARGS(wp->propTermStartLsn),
		   wp->donor->host, wp->donor->port);

	/*
	 * Ensure the safekeeper's WAL is not *behind* the basebackup we are running
	 * (at RedoStartLsn). If it were, streaming would begin before our redo
	 * point, leaving a gap the compute can't fill — clog and other non-rel data
	 * would be inconsistent, so we bail out and retake the basebackup.
	 *
	 * The safekeeper being *ahead* is the normal reconnect/restart case: yb-amp
	 * has a single safekeeper and thus a single compute lineage (no competing
	 * proposer), so resuming from a point past the basebackup is a legitimate
	 * continuation — the compute replays [redo_start, propTermStartLsn) from the
	 * safekeeper before writing new WAL. (This replaces the old term-based
	 * "was this my term?" escape hatch, which is always true for a single SK.)
	 */
	walprop_shared = wp->api.get_shmem_state(wp);
	if (!wp->config->syncSafekeepers && !walprop_shared->replica_promote)
	{
		/*
		 * Basebackup LSN always points to the beginning of the record (not
		 * the page), as StartupXLOG most probably wants it this way.
		 * Safekeepers don't skip header as they need continious stream of
		 * data, so correct LSN for comparison.
		 */
		if (SkipXLogPageHeader(wp, wp->propTermStartLsn) < wp->api.get_redo_start_lsn(wp))
		{
			/*
			 * Panic to restart PG as we need to retake basebackup. However,
			 * don't dump core as this is kinda expected scenario.
			 */
			disable_core_dump();
			wp_log(PANIC,
				   "safekeeper end-of-WAL %X/%X is behind basebackup LSN %X/%X",
				   LSN_FORMAT_ARGS(wp->propTermStartLsn),
				   LSN_FORMAT_ARGS(wp->api.get_redo_start_lsn(wp)));
		}
	}
}

/*
 * Send the ProposerElected message to the safekeeper, announcing the point
 * from which we will stream (the safekeeper truncates its WAL beyond it).
 *
 * Sets sk->startStreamingAt.
 */
static void
SendProposerElected(Safekeeper *sk)
{
	WalProposer *wp = sk->wp;
	ProposerElected msg;

	/* Now that we are ready to send it's a good moment to create WAL reader */
	wp->api.wal_reader_allocate(sk);

	/* Single safekeeper: resume streaming at its reported end-of-WAL. */
	sk->startStreamingAt = wp->propTermStartLsn;

	Assert(sk->startStreamingAt <= wp->availableLsn);

	msg.apm.tag = 'e';
	msg.startStreamingAt = sk->startStreamingAt;

	wp_log(LOG,
		   "sending elected msg to node " UINT64_FORMAT ", startStreamingAt=%X/%X to %s:%s",
		   sk->greetResponse.nodeId, LSN_FORMAT_ARGS(msg.startStreamingAt),
		   sk->host, sk->port);

	PAMessageSerialize(wp, (ProposerAcceptorMessage *) &msg, &sk->outbuf, wp->config->proto_version);
	if (!AsyncWrite(sk, sk->outbuf.data, sk->outbuf.len, SS_SEND_ELECTED_FLUSH))
		return;

	StartStreaming(sk);
}

/*
 * Start streaming to safekeeper sk, always updates state to SS_ACTIVE and sets
 * correct event set.
 */
static void
StartStreaming(Safekeeper *sk)
{
	/*
	 * This is the only entrypoint to state SS_ACTIVE. It's executed exactly
	 * once for a connection.
	 */
	sk->state = SS_ACTIVE;
	sk->active_state = SS_ACTIVE_SEND;
	sk->streamingAt = sk->startStreamingAt;

	/* BEGIN_HADRON */
	sk->wp->api.update_safekeeper_status_for_metrics(sk->wp, sk->index, 1);
	/* END_HADRON */

	/*
	 * Donors can only be in SS_ACTIVE state, so we potentially update the
	 * donor when we switch one to SS_ACTIVE.
	 */
	UpdateDonorShmem(sk->wp);

	/* event set will be updated inside SendMessageToNode */
	SendMessageToNode(sk);
}

/*
 * Try to send message to the particular node. Always updates event set. Will
 * send at least one message, if socket is ready.
 *
 * Can be used only for safekeepers in SS_ACTIVE state. State can be changed
 * in case of errors.
 */
static void
SendMessageToNode(Safekeeper *sk)
{
	Assert(sk->state == SS_ACTIVE);

	/*
	 * Note: we always send everything to the safekeeper until WOULDBLOCK or
	 * nothing left to send
	 */
	HandleActiveState(sk, WL_SOCKET_WRITEABLE);
}

/*
 * Broadcast new message to all caught-up safekeepers
 */
static void
BroadcastAppendRequest(WalProposer *wp)
{
	for (int i = 0; i < wp->n_safekeepers; i++)
		if (wp->safekeeper[i].state == SS_ACTIVE)
			SendMessageToNode(&wp->safekeeper[i]);
}

static void
PrepareAppendRequest(WalProposer *wp, AppendRequestHeader *req, XLogRecPtr beginLsn, XLogRecPtr endLsn)
{
	Assert(endLsn >= beginLsn);
	req->apm.tag = 'a';
	req->beginLsn = beginLsn;
	req->endLsn = endLsn;
	/* yb-mode: generation / term / commit_lsn / truncate_lsn removed from the wire. */
}

/*
 * Process all events happened in SS_ACTIVE state, update event set after that.
 */
static void
HandleActiveState(Safekeeper *sk, uint32 events)
{
	WalProposer *wp = sk->wp;

	/*
	 * Note: we don't known which socket awoke us (sk or nwr). However, as
	 * SendAppendRequests always tries to send at least one msg in
	 * SS_ACTIVE_SEND be careful not to go there if are only after sk
	 * response, otherwise it'd create busy loop of pings.
	 */
	if (events & WL_SOCKET_WRITEABLE || sk->active_state == SS_ACTIVE_READ_WAL)
		if (!SendAppendRequests(sk))
			return;

	if (events & WL_SOCKET_READABLE)
		if (!RecvAppendResponses(sk))
			return;

#if PG_VERSION_NUM >= 150000
	/* expected never to happen, c.f. walprop_pg_active_state_update_event_set */
	if (events & WL_SOCKET_CLOSED)
	{
		wp_log(WARNING, "connection to %s:%s in active state failed, got WL_SOCKET_CLOSED on yb_amp_walreader socket",
			   sk->host, sk->port);
		ShutdownConnection(sk);
		return;
	}
#endif

	/* configures event set for yield whatever is the substate */
	wp->api.active_state_update_event_set(sk);
}

/*
 * Send WAL messages starting from sk->streamingAt until the end or non-writable
 * socket or yb_amp_walreader blocks, whichever comes first; active_state is
 * updated accordingly. Caller should take care of updating event set. Even if
 * no unsent WAL is available, at least one empty message will be sent as a
 * heartbeat, if socket is ready.
 *
 * Resets state and kills the connections if any error on them is encountered.
 * Returns false in this case, true otherwise.
 */
static bool
SendAppendRequests(Safekeeper *sk)
{
	WalProposer *wp = sk->wp;
	XLogRecPtr	endLsn;
	PGAsyncWriteResult writeResult;
	bool		sentAnything = false;
	AppendRequestHeader *req;

	if (sk->active_state == SS_ACTIVE_FLUSH)
	{
		if (!AsyncFlush(sk))

			/*
			 * AsyncFlush failed, that could happen if the socket is closed or
			 * we have nothing to write and should wait for writeable socket.
			 */
			return sk->state == SS_ACTIVE;

		/* Event set will be updated in the end of HandleActiveState */
		sk->active_state = SS_ACTIVE_SEND;
	}

	while (sk->streamingAt != wp->availableLsn || !sentAnything)
	{
		if (sk->active_state == SS_ACTIVE_SEND)
		{
			sentAnything = true;

			endLsn = sk->streamingAt;
			endLsn += MAX_SEND_SIZE;

			/* if we went beyond available WAL, back off */
			if (endLsn > wp->availableLsn)
			{
				endLsn = wp->availableLsn;
			}

			req = &sk->appendRequest;
			PrepareAppendRequest(sk->wp, &sk->appendRequest, sk->streamingAt, endLsn);

			wp_log(DEBUG5, "sending message len %ld beginLsn=%X/%X endLsn=%X/%X to %s:%s",
				   req->endLsn - req->beginLsn,
				   LSN_FORMAT_ARGS(req->beginLsn),
				   LSN_FORMAT_ARGS(req->endLsn),
				   sk->host, sk->port);

			resetStringInfo(&sk->outbuf);

			/* write AppendRequest header */
			PAMessageSerialize(wp, (ProposerAcceptorMessage *) req, &sk->outbuf, wp->config->proto_version);
			/* prepare for reading WAL into the outbuf */
			enlargeStringInfo(&sk->outbuf, req->endLsn - req->beginLsn);
			sk->active_state = SS_ACTIVE_READ_WAL;
		}

		if (sk->active_state == SS_ACTIVE_READ_WAL)
		{
			char	   *errmsg;
			int			req_len;

			req = &sk->appendRequest;
			req_len = req->endLsn - req->beginLsn;

			/*
			 * We send zero sized AppenRequests as heartbeats; don't wal_read
			 * for these.
			 */
			if (req_len > 0)
			{
				switch (wp->api.wal_read(sk,
										 &sk->outbuf.data[sk->outbuf.len],
										 req->beginLsn,
										 req_len,
										 &errmsg))
				{
					case YBAMP_WALREAD_SUCCESS:
						break;
					case YBAMP_WALREAD_WOULDBLOCK:
						return true;
					case YBAMP_WALREAD_ERROR:
						wp_log(WARNING, "WAL reading for node %s:%s failed: %s",
							   sk->host, sk->port, errmsg);
						ShutdownConnection(sk);
						return false;
					default:
						Assert(false);
				}
			}

			sk->outbuf.len += req_len;

			writeResult = wp->api.conn_async_write(sk, sk->outbuf.data, sk->outbuf.len);

			/* Mark current message as sent, whatever the result is */
			sk->streamingAt = req->endLsn;

			switch (writeResult)
			{
				case PG_ASYNC_WRITE_SUCCESS:
					/* Continue writing the next message */
					sk->active_state = SS_ACTIVE_SEND;
					break;

				case PG_ASYNC_WRITE_TRY_FLUSH:

					/*
					 * We still need to call PQflush some more to finish the
					 * job. Caller function will handle this by setting right
					 * event set.
					 */
					sk->active_state = SS_ACTIVE_FLUSH;
					return true;

				case PG_ASYNC_WRITE_FAIL:
					wp_log(WARNING, "failed to send to node %s:%s in %s state: %s",
						   sk->host, sk->port, FormatSafekeeperState(sk),
						   wp->api.conn_error_message(sk));
					ShutdownConnection(sk);
					return false;
				default:
					Assert(false);
					return false;
			}
		}
	}

	return true;
}

/*
 * Receive and process all available feedback.
 *
 * Resets state and kills the connection if any error on it is encountered.
 * Returns false in this case, true otherwise.
 *
 * NB: This function can call SendMessageToNode and produce new messages.
 */
static bool
RecvAppendResponses(Safekeeper *sk)
{
	WalProposer *wp = sk->wp;
	bool		readAnything = false;

	while (true)
	{
		/*
		 * If our reading doesn't immediately succeed, any necessary error
		 * handling or state setting is taken care of. We can leave any other
		 * work until later.
		 */
		sk->appendResponse.apm.tag = 'a';
		if (!AsyncReadMessage(sk, (AcceptorProposerMessage *) &sk->appendResponse))
			break;

		wp_log(DEBUG2, "received message flushLsn=%X/%X from %s:%s",
			   LSN_FORMAT_ARGS(sk->appendResponse.flushLsn),
			   sk->host, sk->port);

		readAnything = true;

		HandleSafekeeperResponse(wp, sk);
	}

	if (!readAnything)
		return sk->state == SS_ACTIVE;

	return sk->state == SS_ACTIVE;
}

#define psfeedback_log(fmt, key, ...) \
	wp_log(DEBUG2, "ParsePageserverFeedbackMessage: %s " fmt, key, __VA_ARGS__)

/* Parse a PageserverFeedback message, or the PageserverFeedback part of an AppendResponse */
static void
ParsePageserverFeedbackMessage(WalProposer *wp, StringInfo reply_message, PageserverFeedback *ps_feedback)
{
	uint8		nkeys;
	int			i;

	/* initialize the struct before parsing */
	memset(ps_feedback, 0, sizeof(PageserverFeedback));
	ps_feedback->present = true;

	/* get number of custom keys */
	nkeys = pq_getmsgbyte(reply_message);

	for (i = 0; i < nkeys; i++)
	{
		const char *key = pq_getmsgrawstring(reply_message);
		unsigned int value_len = pq_getmsgint(reply_message, sizeof(int32));

		if (strcmp(key, "current_timeline_size") == 0)
		{
			Assert(value_len == sizeof(int64));
			ps_feedback->currentClusterSize = pq_getmsgint64(reply_message);
			psfeedback_log(UINT64_FORMAT, key, ps_feedback->currentClusterSize);
		}
		else if ((strcmp(key, "ps_writelsn") == 0) || (strcmp(key, "last_received_lsn") == 0))
		{
			Assert(value_len == sizeof(int64));
			ps_feedback->last_received_lsn = pq_getmsgint64(reply_message);
			psfeedback_log("%X/%X", key, LSN_FORMAT_ARGS(ps_feedback->last_received_lsn));
		}
		else if ((strcmp(key, "ps_flushlsn") == 0) || (strcmp(key, "disk_consistent_lsn") == 0))
		{
			Assert(value_len == sizeof(int64));
			ps_feedback->disk_consistent_lsn = pq_getmsgint64(reply_message);
			psfeedback_log("%X/%X", key, LSN_FORMAT_ARGS(ps_feedback->disk_consistent_lsn));
		}
		else if ((strcmp(key, "ps_applylsn") == 0) || (strcmp(key, "remote_consistent_lsn") == 0))
		{
			Assert(value_len == sizeof(int64));
			ps_feedback->remote_consistent_lsn = pq_getmsgint64(reply_message);
			psfeedback_log("%X/%X", key, LSN_FORMAT_ARGS(ps_feedback->remote_consistent_lsn));
		}
		else if ((strcmp(key, "ps_replytime") == 0) || (strcmp(key, "replytime") == 0))
		{
			Assert(value_len == sizeof(int64));
			ps_feedback->replytime = pq_getmsgint64(reply_message);
			psfeedback_log("%s", key, timestamptz_to_str(ps_feedback->replytime));
		}
		else if (strcmp(key, "corruption_detected") == 0)
		{
			Assert(value_len == 1);
			ps_feedback->corruption_detected = pq_getmsgbyte(reply_message) != 0;
			psfeedback_log("%s", key, ps_feedback->corruption_detected ? "true" : "false");
		}
		else
		{
			/*
			 * Skip unknown keys to support backward compatibile protocol
			 * changes
			 */
			wp_log(LOG, "ParsePageserverFeedbackMessage: unknown key: %s len %d", key, value_len);
			pq_getmsgbytes(reply_message, value_len);
		};
	}
}

/*
 * Calculate WAL position acknowledged as committed. With a single safekeeper
 * whose WAL is durably backed by YugabyteDB, that is simply its reported
 * flushLsn (once it has caught up to our streaming start point).
 */
static XLogRecPtr
GetAcknowledgedByQuorumWALPosition(WalProposer *wp)
{
	Safekeeper *sk = &wp->safekeeper[0];

	/*
	 * Ignore reported LSN until it reaches propTermStartLsn: before the first
	 * ack flushLsn is 0, and we don't commit anything before our start point.
	 */
	return sk->appendResponse.flushLsn >= wp->propTermStartLsn
		? sk->appendResponse.flushLsn
		: 0;
}

/*
 * Return safekeeper with active connection from which WAL can be downloaded, or
 * none if it doesn't exist. donor_lsn is set to end position of the donor to
 * the best of our knowledge.
 */
static void
UpdateDonorShmem(WalProposer *wp)
{
	Safekeeper *donor = NULL;
	int			i;
	XLogRecPtr	donor_lsn = InvalidXLogRecPtr;

	if (wp->state < WPS_ELECTED)
	{
		wp_log(WARNING, "UpdateDonorShmem called before elections are won");
		return;
	}

	/*
	 * First, consider node which had determined our term start LSN as we know
	 * about its position immediately after election before any feedbacks are
	 * sent.
	 */
	if (wp->donor->state >= SS_ACTIVE)
	{
		donor = wp->donor;
		donor_lsn = wp->propTermStartLsn;
	}

	/*
	 * But also check feedbacks from all nodes with live connections and take
	 * the highest one. Note: if node sends feedbacks it already processed
	 * elected message so its term is fine.
	 */
	for (i = 0; i < wp->n_safekeepers; i++)
	{
		Safekeeper *sk = &wp->safekeeper[i];

		if (sk->state == SS_ACTIVE && sk->appendResponse.flushLsn > donor_lsn)
		{
			donor = sk;
			donor_lsn = sk->appendResponse.flushLsn;
		}
	}

	if (donor == NULL)
	{
		wp_log(WARNING, "UpdateDonorShmem didn't find a suitable donor, skipping");
		return;
	}
	wp->api.update_donor(wp, donor, donor_lsn);
}

/*
 * Process AppendResponse message from safekeeper.
 */
static void
HandleSafekeeperResponse(WalProposer *wp, Safekeeper *fromsk)
{
	XLogRecPtr	newCommitLsn;

	newCommitLsn = GetAcknowledgedByQuorumWALPosition(wp);
	if (newCommitLsn > wp->commitLsn)
	{
		wp->commitLsn = newCommitLsn;
		/* Send new value to all safekeepers. */
		BroadcastAppendRequest(wp);
	}

	/*
	 * Unlock syncrep waiters, update ps_feedback, CheckGracefulShutdown().
	 * The last one will terminate the process if the shutdown is requested
	 * and WAL is committed by the quorum. BroadcastAppendRequest() should be
	 * called to notify safekeepers about the new commitLsn.
	 */
	wp->api.process_safekeeper_feedback(wp, fromsk);

	/* yb-mode: truncateLsn removed — no peer recovery to gate. */

	/*
	 * Generally sync is done when majority reached propTermStartLsn so we
	 * committed it and made the majority aware of it, ensuring they are ready
	 * to give all WAL to pageserver. It would mean whichever majority is
	 * alive, there will be at least one safekeeper who is able to stream WAL
	 * to pageserver to make basebackup possible. However, since at the moment
	 * we don't have any good mechanism of defining the healthy and most
	 * advanced safekeeper who should push the wal into pageserver and
	 * basically the random one gets connected, to prevent hanging basebackup
	 * (due to pageserver connecting to not-synced-safekeeper) we currently
	 * wait for all seemingly alive safekeepers to get synced.
	 */
	if (wp->config->syncSafekeepers)
	{
		for (int i = 0; i < wp->n_safekeepers; i++)
		{
			Safekeeper *sk = &wp->safekeeper[i];
			/* yb-mode: commitLsn ≡ flushLsn (single-SK quorum). */
			bool		synced = sk->appendResponse.flushLsn >= wp->propTermStartLsn;

			/* alive safekeeper which is not synced yet; wait for it */
			if (sk->state != SS_OFFLINE && !synced)
				return;
		}

		if (newCommitLsn >= wp->propTermStartLsn)
		{
			/* A quorum of safekeepers has been synced! */

			/*
			 * Send empty message to broadcast latest truncateLsn to all
			 * safekeepers. This helps to finish next sync-safekeepers
			 * eailier, by skipping recovery step.
			 *
			 * We don't need to wait for response because it doesn't affect
			 * correctness, and TCP should be able to deliver the message to
			 * safekeepers in case of network working properly.
			 */
			BroadcastAppendRequest(wp);

			wp->api.finish_sync_safekeepers(wp, wp->propTermStartLsn);
			/* unreachable */
		}
	}
}

/* Serialize proposer -> acceptor message into buf. yb-light only
 * supports v3; v2 was dropped in Phase 9 Wave 2 to match the
 * safekeeper. */
static void
PAMessageSerialize(WalProposer *wp, ProposerAcceptorMessage *msg, StringInfo buf, int proto_version)
{
	if (proto_version != 3)
		wp_log(FATAL, "unsupported proto_version %d; only v3 is supported", proto_version);

	resetStringInfo(buf);

	pq_sendint8(buf, msg->tag);

	switch (msg->tag)
	{
		case 'g':
			{
				ProposerGreeting *m = (ProposerGreeting *) msg;

				pq_send_ascii_string(buf, m->branch_id);
				pq_sendint32(buf, m->pg_version);
				pq_sendint64(buf, m->system_id);
				pq_sendint32(buf, m->wal_seg_size);
				break;
			}
		case 'e':
			{
				ProposerElected *m = (ProposerElected *) msg;

				pq_sendint64(buf, m->startStreamingAt);
				break;
			}
		case 'a':
			{
				/* Header only; caller appends WAL data after. */
				AppendRequestHeader *m = (AppendRequestHeader *) msg;

				pq_sendint64(buf, m->beginLsn);
				pq_sendint64(buf, m->endLsn);
				break;
			}
		default:
			wp_log(FATAL, "unexpected message type %c to serialize", msg->tag);
	}
}

/*
 * Try to read CopyData message from i'th safekeeper, resetting connection on
 * failure.
 */
static bool
AsyncRead(Safekeeper *sk, char **buf, int *buf_size)
{
	WalProposer *wp = sk->wp;

	switch (wp->api.conn_async_read(sk, buf, buf_size))
	{
		case PG_ASYNC_READ_SUCCESS:
			return true;

		case PG_ASYNC_READ_TRY_AGAIN:
			/* WL_SOCKET_READABLE is always set during copyboth */
			return false;

		case PG_ASYNC_READ_FAIL:
			wp_log(WARNING, "failed to read from node %s:%s in %s state: %s", sk->host,
				   sk->port, FormatSafekeeperState(sk),
				   wp->api.conn_error_message(sk));
			ShutdownConnection(sk);
			return false;
	}
	Assert(false);
	return false;
}

/*
 * Read next message with known type into provided struct, by reading a CopyData
 * block from the safekeeper's postgres connection, returning whether the read
 * was successful.
 *
 * If the read needs more polling, we return 'false' and keep the state
 * unmodified, waiting until it becomes read-ready to try again. If it fully
 * failed, a warning is emitted and the connection is reset.
 *
 * Note: it pallocs if needed, i.e. for AcceptorGreeting and VoteResponse fields.
 */
static bool
AsyncReadMessage(Safekeeper *sk, AcceptorProposerMessage *anymsg)
{
	WalProposer *wp = sk->wp;

	char	   *buf;
	int			buf_size;
	uint8		tag;
	StringInfoData s;

	if (!(AsyncRead(sk, &buf, &buf_size)))
		return false;
	sk->latestMsgReceivedAt = wp->api.get_current_timestamp(wp);

	/* parse it */
	s.data = buf;
	s.len = buf_size;
	s.maxlen = buf_size;
	s.cursor = 0;

	if (wp->config->proto_version != 3)
	{
		wp_log(FATAL, "unsupported proto_version %d; only v3 is supported",
			   wp->config->proto_version);
		return false;
	}

	tag = pq_getmsgbyte(&s);
	if (tag != anymsg->tag)
	{
		wp_log(WARNING, "unexpected message tag %c from node %s:%s in state %s", (char) tag, sk->host,
			   sk->port, FormatSafekeeperState(sk));
		ResetConnection(sk);
		return false;
	}
	switch (tag)
	{
		case 'g':
			{
				AcceptorGreeting *msg = (AcceptorGreeting *) anymsg;

				msg->nodeId = pq_getmsgint64(&s);
				msg->flushLsn = pq_getmsgint64(&s);
				pq_getmsgend(&s);
				return true;
			}
		case 'a':
			{
				AppendResponse *msg = (AppendResponse *) anymsg;

				msg->flushLsn = pq_getmsgint64(&s);
				msg->hs.ts = pq_getmsgint64(&s);
				msg->hs.xmin.value = pq_getmsgint64(&s);
				msg->hs.catalog_xmin.value = pq_getmsgint64(&s);
				if (s.len > s.cursor)
					ParsePageserverFeedbackMessage(wp, &s, &msg->ps_feedback);
				else
					msg->ps_feedback.present = false;
				pq_getmsgend(&s);
				return true;
			}
		default:
			{
				wp_log(FATAL, "unexpected message tag %c to read", (char) tag);
				return false;
			}
	}
}

/*
 * Blocking equivalent to AsyncWrite.
 *
 * We use this everywhere messages are small enough that they should fit in a
 * single packet.
 */
static bool
BlockingWrite(Safekeeper *sk, void *msg, size_t msg_size, SafekeeperState success_state)
{
	WalProposer *wp = sk->wp;
	uint32		sk_events;
	uint32		nwr_events;

	if (!wp->api.conn_blocking_write(sk, msg, msg_size))
	{
		wp_log(WARNING, "failed to send to node %s:%s in %s state: %s",
			   sk->host, sk->port, FormatSafekeeperState(sk),
			   wp->api.conn_error_message(sk));
		ShutdownConnection(sk);
		return false;
	}

	sk->state = success_state;

	/*
	 * If the new state will be waiting for events to happen, update the event
	 * set to wait for those
	 */
	SafekeeperStateDesiredEvents(sk, &sk_events, &nwr_events);

	/*
	 * nwr_events is relevant only during SS_ACTIVE which doesn't use
	 * BlockingWrite
	 */
	Assert(!nwr_events);
	if (sk_events)
		wp->api.update_event_set(sk, sk_events);

	return true;
}

/*
 * Starts a write into the 'i'th safekeeper's postgres connection, moving to
 * flush_state (adjusting eventset) if write still needs flushing.
 *
 * Returns false if sending is unfinished (requires flushing or conn failed).
 * Upon failure, a warning is emitted and the connection is reset.
 */
static bool
AsyncWrite(Safekeeper *sk, void *msg, size_t msg_size, SafekeeperState flush_state)
{
	WalProposer *wp = sk->wp;

	switch (wp->api.conn_async_write(sk, msg, msg_size))
	{
		case PG_ASYNC_WRITE_SUCCESS:
			return true;
		case PG_ASYNC_WRITE_TRY_FLUSH:

			/*
			 * We still need to call PQflush some more to finish the job; go
			 * to the appropriate state. Update the event set at the bottom of
			 * this function
			 */
			sk->state = flush_state;
			wp->api.update_event_set(sk, WL_SOCKET_READABLE | WL_SOCKET_WRITEABLE);
			return false;
		case PG_ASYNC_WRITE_FAIL:
			wp_log(WARNING, "failed to send to node %s:%s in %s state: %s",
				   sk->host, sk->port, FormatSafekeeperState(sk),
				   wp->api.conn_error_message(sk));
			ShutdownConnection(sk);
			return false;
		default:
			Assert(false);
			return false;
	}
}

/*
 * Flushes a previous call to AsyncWrite. This only needs to be called when the
 * socket becomes read or write ready *after* calling AsyncWrite.
 *
 * If flushing successfully completes returns true, otherwise false. Event set
 * is updated only if connection fails, otherwise caller should manually unset
 * WL_SOCKET_WRITEABLE.
 */
static bool
AsyncFlush(Safekeeper *sk)
{
	WalProposer *wp = sk->wp;

	/*---
	 * PQflush returns:
	 *   0 if successful                    [we're good to move on]
	 *   1 if unable to send everything yet [call PQflush again]
	 *  -1 if it failed                     [emit an error]
	 */
	switch (wp->api.conn_flush(sk))
	{
		case 0:
			/* flush is done */
			return true;
		case 1:
			/* Nothing to do; try again when the socket's ready */
			return false;
		case -1:
			wp_log(WARNING, "failed to flush write to node %s:%s in %s state: %s",
				   sk->host, sk->port, FormatSafekeeperState(sk),
				   wp->api.conn_error_message(sk));
			ResetConnection(sk);
			return false;
		default:
			Assert(false);
			return false;
	}
}

/* Returns a human-readable string corresonding to the SafekeeperState
 *
 * The string should not be freed.
 *
 * The strings are intended to be used as a prefix to "state", e.g.:
 *
 *   wp_log(LOG, "currently in %s state", FormatSafekeeperState(sk));
 *
 * If this sort of phrasing doesn't fit the message, instead use something like:
 *
 *   wp_log(LOG, "currently in state [%s]", FormatSafekeeperState(sk));
 */
static char *
FormatSafekeeperState(Safekeeper *sk)
{
	char	   *return_val = NULL;

	switch (sk->state)
	{
		case SS_OFFLINE:
			return_val = "offline";
			break;
		case SS_CONNECTING_READ:
		case SS_CONNECTING_WRITE:
			return_val = "connecting";
			break;
		case SS_WAIT_EXEC_RESULT:
			return_val = "receiving query result";
			break;
		case SS_HANDSHAKE_RECV:
			return_val = "handshake (receiving)";
			break;
		case SS_SEND_ELECTED_FLUSH:
			return_val = "send-announcement-flush";
			break;
		case SS_ACTIVE:
			switch (sk->active_state)
			{
				case SS_ACTIVE_SEND:
					return_val = "active send";
					break;
				case SS_ACTIVE_READ_WAL:
					return_val = "active read WAL";
					break;
				case SS_ACTIVE_FLUSH:
					return_val = "active flush";
					break;
			}
			break;
	}

	Assert(return_val != NULL);

	return return_val;
}

/* Asserts that the provided events are expected for given safekeeper's state */
static void
AssertEventsOkForState(uint32 events, Safekeeper *sk)
{
	uint32		sk_events;
	uint32		nwr_events;
	uint32		expected;
	bool		events_ok_for_state;	/* long name so the `Assert` is more
										 * clear later */
	WalProposer *wp = sk->wp;

	SafekeeperStateDesiredEvents(sk, &sk_events, &nwr_events);

	/*
	 * Without one more level of notify target indirection we have no way to
	 * distinguish which socket woke up us, so just union expected events.
	 */
	expected = sk_events | nwr_events;
	events_ok_for_state = ((events & expected) != 0);

	if (!events_ok_for_state)
	{
		/*
		 * To give a descriptive message in the case of failure, we use elog
		 * and then an assertion that's guaranteed to fail.
		 */
		wp_log(WARNING, "events %s mismatched for safekeeper %s:%s in state [%s]",
			   FormatEvents(wp, events), sk->host, sk->port, FormatSafekeeperState(sk));
		Assert(events_ok_for_state);
	}
}

/* Returns the set of events for both safekeeper (sk_events) and yb_amp_walreader
 * (nwr_events) sockets a safekeeper in this state should be waiting on.
 *
 * This will return WL_NO_EVENTS (= 0) for some events. */
void
SafekeeperStateDesiredEvents(Safekeeper *sk, uint32 *sk_events, uint32 *nwr_events)
{
	WalProposer *wp = sk->wp;

	*nwr_events = 0;			/* nwr_events is empty for most states */

	/* If the state doesn't have a modifier, we can check the base state */
	switch (sk->state)
	{
			/* Connecting states say what they want in the name */
		case SS_CONNECTING_READ:
			*sk_events = WL_SOCKET_READABLE;
			return;
		case SS_CONNECTING_WRITE:
			*sk_events = WL_SOCKET_WRITEABLE;
			return;

			/* Reading states need the socket to be read-ready to continue */
		case SS_WAIT_EXEC_RESULT:
		case SS_HANDSHAKE_RECV:
			*sk_events = WL_SOCKET_READABLE;
			return;

		case SS_SEND_ELECTED_FLUSH:
			*sk_events = WL_SOCKET_READABLE | WL_SOCKET_WRITEABLE;
			return;

		case SS_ACTIVE:
			switch (sk->active_state)
			{
					/*
					 * Everything is sent; we just wait for sk responses and
					 * latch.
					 *
					 * Note: this assumes we send all available WAL to
					 * safekeeper in one wakeup (unless it blocks). Otherwise
					 * we would want WL_SOCKET_WRITEABLE here to finish the
					 * work.
					 */
				case SS_ACTIVE_SEND:
					*sk_events = WL_SOCKET_READABLE;
					/* c.f. walprop_pg_active_state_update_event_set */
#if PG_VERSION_NUM >= 150000
					if (wp->api.wal_reader_events(sk))
						*nwr_events = WL_SOCKET_CLOSED;
#endif							/* on PG 14 nwr_events remains 0 */
					return;

					/*
					 * Waiting for yb_amp_walreader socket, but we still read
					 * responses from sk socket.
					 */
				case SS_ACTIVE_READ_WAL:
					*sk_events = WL_SOCKET_READABLE;
					*nwr_events = wp->api.wal_reader_events(sk);
					return;

					/*
					 * Need to flush the sk socket, so ignore yb_amp_walreader
					 * one and set write interest on sk.
					 */
				case SS_ACTIVE_FLUSH:
					*sk_events = WL_SOCKET_READABLE | WL_SOCKET_WRITEABLE;
#if PG_VERSION_NUM >= 150000
					/* c.f. walprop_pg_active_state_update_event_set */
					if (wp->api.wal_reader_events(sk))
						*nwr_events = WL_SOCKET_CLOSED;
#endif							/* on PG 14 nwr_events remains 0 */
					return;
			}
			return;

			/* The offline state expects no events. */
		case SS_OFFLINE:
			*sk_events = 0;
			return;

		default:
			Assert(false);
	}
}

/* Returns a human-readable string corresponding to the event set
 *
 * If the events do not correspond to something set as the `events` field of a `WaitEvent`, the
 * returned string may be meaingless.
 *
 * The string should not be freed. It should also not be expected to remain the same between
 * function calls. */
static char *
FormatEvents(WalProposer *wp, uint32 events)
{
	static char return_str[8];

	/* Helper variable to check if there's extra bits */
	uint32		all_flags = WL_LATCH_SET
		| WL_SOCKET_READABLE
		| WL_SOCKET_WRITEABLE
		| WL_TIMEOUT
		| WL_POSTMASTER_DEATH
		| WL_EXIT_ON_PM_DEATH
		| WL_SOCKET_CONNECTED;

	/*
	 * The formatting here isn't supposed to be *particularly* useful -- it's
	 * just to give an sense of what events have been triggered without
	 * needing to remember your powers of two.
	 */

	return_str[0] = (events & WL_LATCH_SET) ? 'L' : '_';
	return_str[1] = (events & WL_SOCKET_READABLE) ? 'R' : '_';
	return_str[2] = (events & WL_SOCKET_WRITEABLE) ? 'W' : '_';
	return_str[3] = (events & WL_TIMEOUT) ? 'T' : '_';
	return_str[4] = (events & WL_POSTMASTER_DEATH) ? 'D' : '_';
	return_str[5] = (events & WL_EXIT_ON_PM_DEATH) ? 'E' : '_';
	return_str[5] = (events & WL_SOCKET_CONNECTED) ? 'C' : '_';

	if (events & (~all_flags))
	{
		wp_log(WARNING, "event formatting found unexpected component %d",
			   events & (~all_flags));
		return_str[6] = '*';
		return_str[7] = '\0';
	}
	else
		return_str[6] = '\0';

	return (char *) &return_str;
}

