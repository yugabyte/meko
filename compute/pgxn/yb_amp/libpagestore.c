/*-------------------------------------------------------------------------
 *
 * libpagestore.c
 *	  Handles network communications with the remote pagestore.
 *
 * Portions Copyright (c) 1996-2021, PostgreSQL Global Development Group
 * Portions Copyright (c) 1994, Regents of the University of California
 *
 *-------------------------------------------------------------------------
 */
#include "postgres.h"

#include <math.h>
#include <sys/socket.h>

#include "libpq-int.h"

#include "access/xlog.h"
#include "common/hashfn.h"
#include "fmgr.h"
#include "libpq-fe.h"
#include "libpq/libpq.h"
#include "libpq/pqformat.h"
#include "miscadmin.h"
#include "pgstat.h"
#include "portability/instr_time.h"
#include "postmaster/interrupt.h"
#include "storage/buf_internals.h"
#include "storage/fd.h"
#include "storage/ipc.h"
#include "storage/lwlock.h"
#include "storage/pg_shmem.h"
#include "utils/guc.h"
#include "utils/memutils.h"

#include "yb_amp.h"
#include "yb_amp_perf_counters.h"
#include "yb_amp_utils.h"
#include "pagestore_client.h"
#include "walproposer.h"

#ifdef __linux__
#include <sys/ioctl.h>
#include <linux/sockios.h>
#endif

#define PageStoreTrace DEBUG5

#define MIN_RECONNECT_INTERVAL_USEC 1000
#define MAX_RECONNECT_INTERVAL_USEC 1000000

enum YbAmpComputeMode {
	CP_MODE_PRIMARY = 0,
	CP_MODE_REPLICA,
	CP_MODE_STATIC
};

static const struct config_enum_entry yb_amp_compute_modes[] = {
	{"primary", CP_MODE_PRIMARY, false},
	{"replica", CP_MODE_REPLICA, false},
	{"static", CP_MODE_STATIC, false},
	{NULL, 0, false}
};

/* GUCs */
char	   *yb_amp_project_id;
char	   *yb_amp_branch_id;
char	   *yb_amp_endpoint_id;
int32		max_cluster_size;
char	   *pageserver_connstring;
char	   *yb_amp_auth_token;

int			readahead_buffer_size = 128;
int			flush_every_n_requests = 8;

int         yb_amp_protocol_version = 3;

static int	yb_amp_compute_mode = 0;
static int	max_reconnect_attempts = 60;
static int	max_sockets;

static int pageserver_response_log_timeout = 10000;
/*
 * 30 s. Below the kernel's default TCP-retransmit ceiling on purpose:
 * yb-pageserver is a same-DC, low-latency hop, so a stuck request is far
 * more likely a pageserver-side stall than transient packet loss, and
 * we want the compute to drop+reopen the socket quickly rather than wait
 * out an arbitrarily long TCP retry sequence.
 */
static int pageserver_response_disconnect_timeout = 30000;

typedef enum PSConnectionState {
	PS_Disconnected,			/* no connection yet */
	PS_Connecting_Startup,		/* connection starting up */
	PS_Connecting_PageStream,	/* negotiating pagestream */
	PS_Connected,				/* connected, pagestream established */
} PSConnectionState;

/* This backend's connection to the page server */
typedef struct
{
	TimestampTz		last_connect_time; /* read-only debug value */
	TimestampTz		last_reconnect_time;
	uint32			delay_us;
	int				n_reconnect_attempts;

	/*---
	 * Pageserver connection state, i.e.
	 *	disconnected: conn == NULL, wes == NULL;
	 *	conn_startup: connection initiated, waiting for connection establishing
	 *	conn_ps:      PageStream query sent, waiting for confirmation
	 *	connected:    PageStream established
	 */
	PSConnectionState state;
	PGconn		   *conn;

	/* request / response counters for debugging */
	uint64			nrequests_sent;
	uint64			nresponses_received;

	/* State for the receive timeout mechanism in call_PQgetCopyData() */
	instr_time		receive_start_time;			/* when we started waiting */
	instr_time		receive_last_log_time;		/* when we last printed a log message for the wait */
	bool			receive_logged;				/* has the wait been logged */

	/*---
	 * WaitEventSet containing:
	 *	- WL_SOCKET_READABLE on 'conn'
	 *	- WL_LATCH_SET on MyLatch, and
	 *	- WL_EXIT_ON_PM_DEATH.
	 */
	WaitEventSet   *wes_read;
} PageServer;

static uint32 local_request_counter;
#define GENERATE_REQUEST_ID() (((YbAmpRequestId)MyProcPid << 32) | ++local_request_counter)

/* There is exactly one page server per endpoint in AMP / yb-light. */
static PageServer page_server;

static void pageserver_disconnect(void);

/*
 * Validate a pageserver connection string. There is exactly one page server
 * per endpoint, so the GUC holds a single connection string.
 *
 * If the input is not valid, returns false.
 */
static bool
CheckPageserverConnstring(char **newval, void **extra, GucSource source)
{
	if (strlen(*newval) >= MAX_PAGESERVER_CONNSTRING_SIZE)
	{
		yb_amp_log(LOG, "Connection string too long");
		return false;
	}

	return true;
}

static inline void
CLEANUP_AND_DISCONNECT(PageServer *ps)
{
	if (ps->wes_read)
	{
		FreeWaitEventSet(ps->wes_read);
		ps->wes_read = NULL;
	}
	if (ps->conn)
	{
		MyYbAmpCounters->pageserver_disconnects_total++;
		PQfinish(ps->conn);
		ps->conn = NULL;
	}

	ps->state = PS_Disconnected;
}

/*
 * Connect to a pageserver, or continue to try to connect if we're yet to
 * complete the connection (e.g. due to receiving an earlier cancellation
 * during connection start).
 * Returns true if successfully connected; false if the connection failed.
 *
 * Throws errors in unrecoverable situations, or when this backend's query
 * is canceled.
 */
static bool
pageserver_connect(int elevel)
{
	PageServer *ps = &page_server;
	char		connstr[MAX_PAGESERVER_CONNSTRING_SIZE];

	/*
	 * The pageserver connection string is fixed for the lifetime of the
	 * endpoint, so we just read it from the GUC. It is used both during
	 * connection start, and when we log the successful connection.
	 */
	strlcpy(connstr, pageserver_connstring, sizeof(connstr));

	/* Reserve a file descriptor for the socket on first use. */
	if (max_sockets < 1)
	{
		max_sockets += 1;
		ReserveExternalFD();
	}

	switch (ps->state)
	{
	case PS_Disconnected:
	{
		const char *keywords[5];
		const char *values[5];
		char pid_str[16] = { 0 };
		char endpoint_str[36] = { 0 };
		int			n_pgsql_params;
		TimestampTz	now;
		int64		us_since_last_attempt;

		/* Make sure we start with a clean slate */
		CLEANUP_AND_DISCONNECT(ps);

		yb_amp_log(DEBUG5, "Connection state: Disconnected");

		now = GetCurrentTimestamp();
		us_since_last_attempt = (int64) (now - ps->last_reconnect_time);

		/*
		 * Make sure we don't do exponential backoff with a constant multiplier
		 * of 0 us, as that doesn't really do much for timeouts...
		 *
		 * cf. https://github.com/neondatabase/neon/issues/7897
		 */
		if (ps->delay_us == 0)
			ps->delay_us = MIN_RECONNECT_INTERVAL_USEC;

		/*
		 * If we did other tasks between reconnect attempts, then we won't
		 * need to wait as long as a full delay.
		 *
		 * This is a loop to protect against interrupted sleeps.
		 */
		while (us_since_last_attempt < ps->delay_us)
		{
			pg_usleep(ps->delay_us - us_since_last_attempt);

			/* At least we should handle cancellations here */
			CHECK_FOR_INTERRUPTS();

			now = GetCurrentTimestamp();
			us_since_last_attempt = (int64) (now - ps->last_reconnect_time);
		}

		/* update the delay metric */
		ps->delay_us = Min(ps->delay_us * 2, MAX_RECONNECT_INTERVAL_USEC);
		ps->last_reconnect_time = now;

		/*
		 * Connect using the connection string we got from the
		 * yb_amp.pageserver_connstring GUC. If the YBAMP_AUTH_TOKEN environment
		 * variable was set, use that as the password.
		 *
		 * The connection options are parsed in the order they're given, so when
		 * we set the password before the connection string, the connection string
		 * can override the password from the env variable. Seems useful, although
		 * we don't currently use that capability anywhere.
		 */
		n_pgsql_params = 0;

		/*
		 * Pageserver logs include this in the connection's tracing span.
		 * This allows for reasier log correlation between compute and pageserver.
		 */
		keywords[n_pgsql_params] = "application_name";
		{
			int ret = snprintf(pid_str, sizeof(pid_str), "%d", MyProcPid);
			if (ret < 0 || ret >= (int)(sizeof(pid_str)))
				elog(FATAL, "stack-allocated buffer too small to hold pid");
		}
		/* lifetime: PQconnectStartParams strdups internally */
		values[n_pgsql_params] = (const char*) pid_str;
		n_pgsql_params++;

		keywords[n_pgsql_params] = "dbname";
		values[n_pgsql_params] = connstr;
		n_pgsql_params++;

		if (yb_amp_auth_token)
		{
			keywords[n_pgsql_params] = "password";
			values[n_pgsql_params] = yb_amp_auth_token;
			n_pgsql_params++;
		}

		{
			bool param_set = false;
			switch (yb_amp_compute_mode)
			{
				case CP_MODE_PRIMARY:
					strncpy(endpoint_str, "-c yb_amp.compute_mode=primary", sizeof(endpoint_str));
					param_set = true;
					break;
				case CP_MODE_REPLICA:
					strncpy(endpoint_str, "-c yb_amp.compute_mode=replica", sizeof(endpoint_str));
					param_set = true;
					break;
				case CP_MODE_STATIC:
					strncpy(endpoint_str, "-c yb_amp.compute_mode=static", sizeof(endpoint_str));
					param_set = true;
					break;
			}
			if (param_set)
			{
				keywords[n_pgsql_params] = "options";
				values[n_pgsql_params] = endpoint_str;
				n_pgsql_params++;
			}
		}

		keywords[n_pgsql_params] = NULL;
		values[n_pgsql_params] = NULL;

		ps->conn = PQconnectStartParams(keywords, values, 1);
		if (PQstatus(ps->conn) == CONNECTION_BAD)
		{
			char	   *msg = pchomp(PQerrorMessage(ps->conn));
			CLEANUP_AND_DISCONNECT(ps);
			ereport(elevel,
					(errcode(ERRCODE_SQLCLIENT_UNABLE_TO_ESTABLISH_SQLCONNECTION),
						errmsg(YBAMP_TAG "could not establish connection to pageserver"),
						errdetail_internal("%s", msg)));
			pfree(msg);
			return false;
		}
		ps->state = PS_Connecting_Startup;
	}
	/* FALLTHROUGH */
	case PS_Connecting_Startup:
	{
		char	   *pagestream_query;
		int			ps_send_query_ret;
		bool		connected = false;
		int poll_result = PGRES_POLLING_WRITING;
		yb_amp_log(DEBUG5, "Connection state: Connecting_Startup");

		do
		{
			switch (poll_result)
			{
			default: /* unknown/unused states are handled as a failed connection */
			case PGRES_POLLING_FAILED:
				{
					char	   *pqerr = PQerrorMessage(ps->conn);
					char	   *msg = NULL;
					yb_amp_log(DEBUG5, "POLLING_FAILED");

					if (pqerr)
						msg = pchomp(pqerr);

					CLEANUP_AND_DISCONNECT(ps);

					if (msg)
					{
						yb_amp_log(elevel,
									   "could not connect to pageserver: %s",
									   msg);
						pfree(msg);
					}
					else
						yb_amp_log(elevel,
									   "could not connect to pageserver");

					return false;
				}
			case PGRES_POLLING_READING:
				/* Sleep until there's something to do */
				while (true)
				{
					int rc = WaitLatchOrSocket(MyLatch,
											   WL_EXIT_ON_PM_DEATH | WL_LATCH_SET | WL_SOCKET_READABLE,
											   PQsocket(ps->conn),
											   0,
											   WAIT_EVENT_YBAMP_PS_STARTING);
					elog(DEBUG5, "PGRES_POLLING_READING=>%d", rc);
					if (rc & WL_LATCH_SET)
					{
						ResetLatch(MyLatch);
						/* query cancellation, backend shutdown */
						CHECK_FOR_INTERRUPTS();
					}
					if (rc & WL_SOCKET_READABLE)
						break;
				}
				/* PQconnectPoll() handles the socket polling state updates */

				break;
			case PGRES_POLLING_WRITING:
				/* Sleep until there's something to do */
				while (true)
				{
					int rc = WaitLatchOrSocket(MyLatch,
											   WL_EXIT_ON_PM_DEATH | WL_LATCH_SET | WL_SOCKET_WRITEABLE,
											   PQsocket(ps->conn),
											   0,
											   WAIT_EVENT_YBAMP_PS_STARTING);
					elog(DEBUG5, "PGRES_POLLING_WRITING=>%d", rc);
					if (rc & WL_LATCH_SET)
					{
						ResetLatch(MyLatch);
						/* query cancellation, backend shutdown */
						CHECK_FOR_INTERRUPTS();
					}
					if (rc & WL_SOCKET_WRITEABLE)
						break;
				}
				/* PQconnectPoll() handles the socket polling state updates */

				break;
			case PGRES_POLLING_OK:
				yb_amp_log(DEBUG5, "POLLING_OK");
				connected = true;
				break;
			}
			poll_result = PQconnectPoll(ps->conn);
			elog(DEBUG5, "PQconnectPoll=>%d", poll_result);
		}
		while (!connected);

		/* No more polling needed; connection succeeded */
		ps->last_connect_time = GetCurrentTimestamp();

#if PG_MAJORVERSION_NUM >= 17
		ps->wes_read = CreateWaitEventSet(NULL, 3);
#else
		ps->wes_read = CreateWaitEventSet(TopMemoryContext, 3);
#endif
		AddWaitEventToSet(ps->wes_read, WL_LATCH_SET, PGINVALID_SOCKET,
						  MyLatch, NULL);
		AddWaitEventToSet(ps->wes_read, WL_EXIT_ON_PM_DEATH, PGINVALID_SOCKET,
						  NULL, NULL);
		AddWaitEventToSet(ps->wes_read, WL_SOCKET_READABLE, PQsocket(ps->conn), NULL, NULL);


		switch (yb_amp_protocol_version)
		{
		case 3:
			pagestream_query = psprintf("pagestream_v3 %s", yb_amp_branch_id);
			break;
		case 2:
			pagestream_query = psprintf("pagestream_v2 %s", yb_amp_branch_id);
			break;
		default:
			elog(ERROR, "unexpected yb_amp_protocol_version %d", yb_amp_protocol_version);
		}

		if (PQstatus(ps->conn) == CONNECTION_BAD)
		{
			char	   *msg = pchomp(PQerrorMessage(ps->conn));

			CLEANUP_AND_DISCONNECT(ps);

			ereport(elevel,
					(errcode(ERRCODE_SQLCLIENT_UNABLE_TO_ESTABLISH_SQLCONNECTION),
						errmsg(YBAMP_TAG "could not establish connection to pageserver"),
						errdetail_internal("%s", msg)));
			pfree(msg);
			return false;
		}

		ps_send_query_ret = PQsendQuery(ps->conn, pagestream_query);
		pfree(pagestream_query);
		if (ps_send_query_ret != 1)
		{
			CLEANUP_AND_DISCONNECT(ps);

			yb_amp_log(elevel, "could not send pagestream command to pageserver");
			return false;
		}

		ps->state = PS_Connecting_PageStream;
	}
	/* FALLTHROUGH */
	case PS_Connecting_PageStream:
	{
		yb_amp_log(DEBUG5, "Connection state: Connecting_PageStream");

		if (PQstatus(ps->conn) == CONNECTION_BAD)
		{
			char	   *msg = pchomp(PQerrorMessage(ps->conn));
			CLEANUP_AND_DISCONNECT(ps);
			ereport(elevel,
					(errcode(ERRCODE_SQLCLIENT_UNABLE_TO_ESTABLISH_SQLCONNECTION),
						errmsg(YBAMP_TAG "could not establish connection to pageserver"),
						errdetail_internal("%s", msg)));
			pfree(msg);
			return false;
		}

		while (PQisBusy(ps->conn))
		{
			WaitEvent	event;

			/* Sleep until there's something to do */
			(void) WaitEventSetWait(ps->wes_read, -1L, &event, 1,
									WAIT_EVENT_YBAMP_PS_CONFIGURING);
			ResetLatch(MyLatch);

			CHECK_FOR_INTERRUPTS();

			/* Data available in socket? */
			if (event.events & WL_SOCKET_READABLE)
			{
				if (!PQconsumeInput(ps->conn))
				{
					char	   *msg = pchomp(PQerrorMessage(ps->conn));

					CLEANUP_AND_DISCONNECT(ps);
					yb_amp_log(elevel, "could not complete handshake with pageserver: %s",
								   msg);
					pfree(msg);
					return false;
				}
			}
		}

		ps->state = PS_Connected;
		ps->nrequests_sent = 0;
		ps->nresponses_received = 0;
		INSTR_TIME_SET_ZERO(ps->receive_start_time);
		INSTR_TIME_SET_ZERO(ps->receive_last_log_time);
		ps->receive_logged = false;
	}
	/* FALLTHROUGH */
	case PS_Connected:
		/*
		 * We successfully connected. Future connections to this PageServer
		 * will do fast retries again, with exponential backoff.
		 */
		ps->delay_us = MIN_RECONNECT_INTERVAL_USEC;

		yb_amp_log(DEBUG5, "Connection state: Connected");
		yb_amp_log(LOG, "libpagestore: connected to '%s' with protocol version %d", connstr, yb_amp_protocol_version);
		return true;
	default:
		yb_amp_log(ERROR, "libpagestore: invalid connection state %d", ps->state);
	}

	pg_unreachable();
}

static void
get_socket_stats(int socketfd, int *sndbuf, int *recvbuf)
{
	*sndbuf = -1;
	*recvbuf = -1;

#ifdef __linux__
	/*
	 * get kernel's send and recv queue size via ioctl
	 * https://elixir.bootlin.com/linux/v6.1.128/source/include/uapi/linux/sockios.h#L25-L27
	 */
	if (socketfd != -1)
	{
		int			ioctl_err;

		ioctl_err = ioctl(socketfd, SIOCOUTQ, sndbuf);
		if (ioctl_err!= 0) {
			*sndbuf = -errno;
		}
		ioctl_err = ioctl(socketfd, FIONREAD, recvbuf);
		if (ioctl_err != 0) {
			*recvbuf = -errno;
		}
	}
#endif
}

/*
 * Tries to get the local port of a socket. Sets 'port' to -1 on error.
 */
static void
get_local_port(int socketfd, int *port)
{
	struct sockaddr_in addr;
	socklen_t addr_len = sizeof(addr);

	memset(&addr, 0, addr_len);
	if (getsockname(socketfd, (struct sockaddr*) &addr, &addr_len) == 0)
	{
		*port = ntohs(addr.sin_port);
	} else {
		*port = -1;
	}
}

/*
 * A wrapper around PQgetCopyData that checks for interrupts while sleeping.
 */
static int
call_PQgetCopyData(char **buffer)
{
	int			ret;
	PageServer *ps = &page_server;
	PGconn	   *pageserver_conn = ps->conn;
	instr_time	now,
				since_start,
				since_last_log;

retry:
	ret = PQgetCopyData(pageserver_conn, buffer, 1 /* async */ );

	if (ret == 0)
	{
		WaitEvent	occurred_event;
		int			noccurred;
		double		log_timeout,
					disconnect_timeout;
		long		timeout;

		/*
		 * Calculate time elapsed since the start, and since the last progress
		 * log message. On first call, remember the start time.
		 */
		INSTR_TIME_SET_CURRENT(now);
		if (INSTR_TIME_IS_ZERO(ps->receive_start_time))
		{
			ps->receive_start_time = now;
			INSTR_TIME_SET_ZERO(since_start);
			ps->receive_last_log_time = now;
			INSTR_TIME_SET_ZERO(since_last_log);
			ps->receive_logged = false;
		}
		else
		{
			since_start = now;
			INSTR_TIME_SUBTRACT(since_start, ps->receive_start_time);
			since_last_log = now;
			INSTR_TIME_SUBTRACT(since_last_log, ps->receive_last_log_time);
		}

		/* Sleep until the log or disconnect timeout is reached. */
		log_timeout = Max(0, (double) pageserver_response_log_timeout - INSTR_TIME_GET_MILLISEC(since_last_log));
		disconnect_timeout = Max(0, (double) pageserver_response_disconnect_timeout - INSTR_TIME_GET_MILLISEC(since_start));
		timeout = (long) ceil(Min(log_timeout, disconnect_timeout));

		noccurred = WaitEventSetWait(ps->wes_read, timeout, &occurred_event, 1,
									 WAIT_EVENT_YBAMP_PS_READ);
		ResetLatch(MyLatch);

		CHECK_FOR_INTERRUPTS();

		/* Data available in socket? */
		if (noccurred > 0 && (occurred_event.events & WL_SOCKET_READABLE) != 0)
		{
			if (!PQconsumeInput(pageserver_conn))
			{
				char	   *msg = pchomp(PQerrorMessage(pageserver_conn));

				yb_amp_log(LOG, "could not get response from pageserver: %s", msg);
				pfree(msg);
				return -1;
			}
			goto retry;
		}

		/* Timeout was reached, or we were interrupted for some other reason */
		INSTR_TIME_SET_CURRENT(now);
		since_last_log = now;
		INSTR_TIME_SUBTRACT(since_last_log, ps->receive_last_log_time);
		since_start = now;
		INSTR_TIME_SUBTRACT(since_start, ps->receive_start_time);

		/*
		 * As a debugging aid, if we don't get a response to a pageserver request
		 * for a long time, print a log message.
		 *
		 * The default yb_amp.pageserver_response_log_timeout value, 10 s, is
		 * very generous. Normally we expect a response in a few
		 * milliseconds. We have metrics to track latencies in normal ranges,
		 * but in the cases that take exceptionally long, it's useful to log
		 * the exact timestamps.
		 */
		if (INSTR_TIME_GET_MILLISEC(since_last_log) >= pageserver_response_log_timeout)
		{
			int			port;
			int			sndbuf;
			int			recvbuf;
			uint64*		max_wait;

			get_local_port(PQsocket(pageserver_conn), &port);
			get_socket_stats(PQsocket(pageserver_conn), &sndbuf, &recvbuf);

			yb_amp_log(LOG,
						   "no response received from pageserver for %0.3f s, still waiting (sent " UINT64_FORMAT " requests, received " UINT64_FORMAT " responses) (socket port=%d sndbuf=%d recvbuf=%d) (conn start=%d end=%d)",
						   INSTR_TIME_GET_DOUBLE(since_start),
						   ps->nrequests_sent, ps->nresponses_received, port, sndbuf, recvbuf,
				           pageserver_conn->inStart, pageserver_conn->inEnd);
			ps->receive_last_log_time = now;
			MyYbAmpCounters->compute_getpage_stuck_requests_total += !ps->receive_logged;
			ps->receive_logged = true;
			max_wait = &MyYbAmpCounters->compute_getpage_max_inflight_stuck_time_ms;
			*max_wait = Max(*max_wait, INSTR_TIME_GET_MILLISEC(since_start));
		}

		/*
		 * If an even longer time has passed without receiving a response from
		 * the pageserver, disconnect.  That triggers a reconnection attempt
		 * in the caller.
		 *
		 * If this happens, the pageserver is likely dead and isn't coming
		 * back, or there's some kind of a network glitch and the connection
		 * is permanently gone. Without this, if the pageserver or the network
		 * connection is dead, it could take a very long time (15 minutes or
		 * more) until the TCP keepalive timeout notices that. Even if we
		 * would in fact get a response if we just waited a little longer,
		 * there's a good chance that we'll get the response sooner by
		 * reconnecting.
		 */
		if (INSTR_TIME_GET_MILLISEC(since_start) >= pageserver_response_disconnect_timeout)
		{
			int 		port;
			get_local_port(PQsocket(pageserver_conn), &port);
			yb_amp_log(LOG, "no response from pageserver for %0.3f s, disconnecting (socket port=%d)",
					   INSTR_TIME_GET_DOUBLE(since_start), port);
			MyYbAmpCounters->compute_getpage_max_inflight_stuck_time_ms = 0;
			pageserver_disconnect();
			return -1;
		}

		goto retry;
	}

	/*
	 * If we logged earlier that the response is taking a long time, log
	 * another message when the response is finally received.
	 */
	if (ps->receive_logged)
	{
		INSTR_TIME_SET_CURRENT(now);
		since_start = now;
		INSTR_TIME_SUBTRACT(since_start, ps->receive_start_time);
		yb_amp_log(LOG,
					   "received response from pageserver after %0.3f s",
					   INSTR_TIME_GET_DOUBLE(since_start));
	}
	INSTR_TIME_SET_ZERO(ps->receive_start_time);
	INSTR_TIME_SET_ZERO(ps->receive_last_log_time);
	ps->receive_logged = false;
	MyYbAmpCounters->compute_getpage_max_inflight_stuck_time_ms = 0;

	return ret;
}

/*
 * Reset prefetch and drop the connection to the page server.
 */
static void
pageserver_disconnect(void)
{
	/*
	 * If the connection to the pageserver is lost, we throw away the whole
	 * prefetch queue. It should not cause big problems, because connection
	 * loss is supposed to be a rare event.
	 */
	prefetch_on_ps_disconnect();

	pageserver_disconnect_conn();
}

/*
 * Drop the connection to the page server.
 */
void
pageserver_disconnect_conn(void)
{
	PageServer *ps = &page_server;
	/*
	 * If anything goes wrong while we were sending a request, it's not clear
	 * what state the connection is in. For example, if we sent the request
	 * but didn't receive a response yet, we might receive the response some
	 * time later after we have already sent a new unrelated request. Close
	 * the connection to avoid getting confused.
	 * Similarly, even when we're in PS_DISCONNECTED, we may have junk to
	 * clean up: It is possible that we encountered an error allocating any
	 * of the wait event sets or the psql connection, or failed when we tried
	 * to attach wait events to the WaitEventSets.
	 */
	CLEANUP_AND_DISCONNECT(ps);

	ps->state = PS_Disconnected;
}

bool
pageserver_send(YbAmpRequest *request)
{
	StringInfoData req_buff;
	PageServer *ps = &page_server;
	PGconn	   *pageserver_conn;

	MyYbAmpCounters->pageserver_requests_sent_total++;

	/* If the connection was lost for some reason, reconnect */
	if (ps->state == PS_Connected && PQstatus(ps->conn) == CONNECTION_BAD)
	{
		yb_amp_log(LOG, "pageserver_send disconnect bad connection");
		pageserver_disconnect();
		pageserver_conn = NULL;
	}

	request->reqid = GENERATE_REQUEST_ID();
	req_buff = nm_pack_request(request);

	/*
	 * If pageserver is stopped, the connections from compute node are broken.
	 * The compute node doesn't notice that immediately, but it will cause the
	 * next request to fail, usually on the next query. That causes
	 * user-visible errors if pageserver is restarted, or the tenant is moved
	 * from one pageserver to another. See
	 * https://github.com/neondatabase/neon/issues/1138 So try to reestablish
	 * connection in case of failure.
	 */
	if (ps->state != PS_Connected)
	{
		while (!pageserver_connect(ps->n_reconnect_attempts < max_reconnect_attempts ? LOG : ERROR))
		{
			ps->n_reconnect_attempts += 1;
		}
		ps->n_reconnect_attempts = 0;
	} else {
		Assert(ps->conn != NULL);
	}

	pageserver_conn = ps->conn;

	/*
	 * Send request.
	 *
	 * In principle, this could block if the output buffer is full, and we
	 * should use async mode and check for interrupts while waiting. In
	 * practice, our requests are small enough to always fit in the output and
	 * TCP buffer.
	 *
	 * Note that this also will fail when the connection is in the
	 * PGRES_POLLING_WRITING state. It's kinda dirty to disconnect at this
	 * point, but on the grand scheme of things it's only a small issue.
	 */
	ps->nrequests_sent++;
	if (PQputCopyData(pageserver_conn, req_buff.data, req_buff.len) <= 0)
	{
		char	   *msg = pchomp(PQerrorMessage(pageserver_conn));

		pageserver_disconnect();
		yb_amp_log(LOG, "pageserver_send disconnected: failed to send page request (try to reconnect): %s", msg);
		pfree(msg);
		pfree(req_buff.data);
		return false;
	}

	pfree(req_buff.data);

	if (message_level_is_interesting(PageStoreTrace))
	{
		char	   *msg = nm_to_string((YbAmpMessage *) request);

		yb_amp_log(PageStoreTrace, "sent request: %s", msg);
		pfree(msg);
	}

	return true;
}

YbAmpResponse *
pageserver_receive(void)
{
	StringInfoData resp_buff;
	YbAmpResponse *resp;
	PageServer *ps = &page_server;
	PGconn	   *pageserver_conn = ps->conn;
	/* read response */
	int			rc;

	if (ps->state != PS_Connected)
	{
		yb_amp_log(LOG,
					   "pageserver_receive: returning NULL for non-connected pageserver connection: 0x%02x",
					   ps->state);
		return NULL;
	}

	Assert(pageserver_conn);

	rc = call_PQgetCopyData(&resp_buff.data);
	if (rc >= 0)
	{
		/* call_PQgetCopyData handles rc == 0 */
		Assert(rc > 0);

		PG_TRY();
		{
			resp_buff.len = rc;
			resp_buff.cursor = 0;
			resp = nm_unpack_response(&resp_buff);
			PQfreemem(resp_buff.data);
		}
		PG_CATCH();
		{
			yb_amp_log(LOG, "pageserver_receive: disconnect due to failure while parsing response");
			pageserver_disconnect();
			PG_RE_THROW();
		}
		PG_END_TRY();

		if (message_level_is_interesting(PageStoreTrace))
		{
			char	   *msg = nm_to_string((YbAmpMessage *) resp);

			yb_amp_log(PageStoreTrace, "got response: %s", msg);
			pfree(msg);
		}

		/*
		 * GoingAway is an unsolicited frame: the pageserver is closing
		 * the connection on graceful shutdown. Drop the connection now
		 * so the caller retries via the reconnect loop, instead of
		 * waiting out pageserver_response_disconnect_timeout for the
		 * read to time out.
		 */
		if (resp != NULL && resp->tag == T_YbAmpGoingAwayResponse)
		{
			YbAmpGoingAwayResponse *ga = (YbAmpGoingAwayResponse *) resp;

			yb_amp_log(LOG,
						   "received GoingAway from pageserver (reason=%u); disconnecting",
						   ga->reason);
			pfree(resp);
			pageserver_disconnect();
			ps->nresponses_received++;
			return NULL;
		}
	}
	else if (rc == -1 && ps->state == PS_Disconnected)
	{
		/* If the state is 'Disconnected', the disconnection message was already logged */
		resp = NULL;
	}
	else if (rc == -1)
	{
		char	   *msg = pchomp(PQerrorMessage(pageserver_conn));

		yb_amp_log(LOG, "pageserver_receive disconnect: psql end of copy data: %s", msg);
		pfree(msg);
		pageserver_disconnect();
		resp = NULL;
	}
	else if (rc == -2)
	{
		char	   *msg = pchomp(PQerrorMessage(pageserver_conn));

		pageserver_disconnect();
		yb_amp_log(ERROR, "pageserver_receive disconnect: could not read COPY data: %s", msg);
	}
	else
	{
		pageserver_disconnect();
		yb_amp_log(ERROR, "pageserver_receive disconnect: unexpected PQgetCopyData return value: %d", rc);
	}

	ps->nresponses_received++;
	return (YbAmpResponse *) resp;
}

YbAmpResponse *
pageserver_try_receive(void)
{
	StringInfoData resp_buff;
	YbAmpResponse *resp;
	PageServer *ps = &page_server;
	PGconn	   *pageserver_conn = ps->conn;
	int	rc;

	if (ps->state != PS_Connected)
		return NULL;

	Assert(pageserver_conn);

	rc = PQgetCopyData(ps->conn, &resp_buff.data, 1 /* async */);
	if (rc == 0)
	{
		if (!PQconsumeInput(ps->conn))
		{
			return NULL;
		}
		rc = PQgetCopyData(ps->conn, &resp_buff.data, 1 /* async */);
	}

	if (rc == 0)
		return NULL;
	else if (rc > 0)
	{
		PG_TRY();
		{
			resp_buff.len = rc;
			resp_buff.cursor = 0;
			resp = nm_unpack_response(&resp_buff);
			PQfreemem(resp_buff.data);
		}
		PG_CATCH();
		{
			yb_amp_log(LOG, "pageserver_receive: disconnect due to failure while parsing response");
			pageserver_disconnect();
			/*
			 * Malformed responses from PageServer are a reason to raise
			 * errors and cancel transactions.
			 */
			PG_RE_THROW();
		}
		PG_END_TRY();

		if (message_level_is_interesting(PageStoreTrace))
		{
			char	   *msg = nm_to_string((YbAmpMessage *) resp);

			yb_amp_log(PageStoreTrace, "got response: %s", msg);
			pfree(msg);
		}

		if (resp != NULL && resp->tag == T_YbAmpGoingAwayResponse)
		{
			YbAmpGoingAwayResponse *ga = (YbAmpGoingAwayResponse *) resp;

			yb_amp_log(LOG,
						   "received GoingAway from pageserver (reason=%u); disconnecting",
						   ga->reason);
			pfree(resp);
			pageserver_disconnect();
			return NULL;
		}
	}
	else if (rc == -1)
	{
		yb_amp_log(LOG, "pageserver_receive disconnect: psql end of copy data: %s", pchomp(PQerrorMessage(pageserver_conn)));
		pageserver_disconnect();
		resp = NULL;
	}
	else if (rc == -2)
	{
		char	   *msg = pchomp(PQerrorMessage(pageserver_conn));

		pageserver_disconnect();
		yb_amp_log(LOG, "pageserver_receive disconnect: could not read COPY data: %s", msg);
		resp = NULL;
	}
	else
	{
		pageserver_disconnect();
		yb_amp_log(ERROR, "pageserver_receive disconnect: unexpected PQgetCopyData return value: %d", rc);
	}

	ps->nresponses_received++;
	return (YbAmpResponse *) resp;
}


bool
pageserver_flush(void)
{
	PGconn	   *pageserver_conn = page_server.conn;

	if (page_server.state != PS_Connected)
	{
		yb_amp_log(WARNING, "Tried to flush while disconnected");
	}
	else
	{
		MyYbAmpCounters->pageserver_send_flushes_total++;
		if (PQflush(pageserver_conn))
		{
			char	   *msg = pchomp(PQerrorMessage(pageserver_conn));

			pageserver_disconnect();
			yb_amp_log(LOG, "pageserver_flush disconnect because failed to flush page requests: %s", msg);
			pfree(msg);
			return false;
		}
	}

	return true;
}

static bool
check_yb_amp_id(char **newval, void **extra, GucSource source)
{
	uint8		id[16];

	return **newval == '\0' || HexDecodeString(id, *newval, 16);
}

/*
 * Module initialization function
 */
void
pg_init_libpagestore(void)
{
	DefineCustomStringVariable("yb_amp.pageserver_connstring",
							   "connection string to the page server",
							   NULL,
							   &pageserver_connstring,
							   "",
							   PGC_SIGHUP,
							   0,	/* no flags required */
							   CheckPageserverConnstring, NULL, NULL);

	DefineCustomStringVariable("yb_amp.project_id",
							   "YbAmp project_id the server is running on",
							   NULL,
							   &yb_amp_project_id,
							   "",
							   PGC_POSTMASTER,
							   0,	/* no flags required */
							   NULL, NULL, NULL);
	DefineCustomStringVariable("yb_amp.branch_id",
							   "YbAmp branch_id the server is running on",
							   NULL,
							   &yb_amp_branch_id,
							   "",
							   PGC_POSTMASTER,
							   0,	/* no flags required */
							   check_yb_amp_id, NULL, NULL);
	DefineCustomStringVariable("yb_amp.endpoint_id",
							   "YbAmp endpoint_id the server is running on",
							   NULL,
							   &yb_amp_endpoint_id,
							   "",
							   PGC_POSTMASTER,
							   0,	/* no flags required */
							   NULL, NULL, NULL);

	DefineCustomIntVariable("yb_amp.max_cluster_size",
							"cluster size limit",
							NULL,
							&max_cluster_size,
							-1, -1, INT_MAX,
							PGC_SIGHUP,
							GUC_UNIT_MB,
							NULL, NULL, NULL);
	DefineCustomIntVariable("yb_amp.flush_output_after",
							"Flush the output buffer after every N unflushed requests",
							NULL,
							&flush_every_n_requests,
							8, -1, INT_MAX,
							PGC_USERSET,
							0,	/* no flags required */
							NULL, NULL, NULL);
	DefineCustomIntVariable("yb_amp.max_reconnect_attempts",
							"Maximal attempts to reconnect to pages server (with 1 second timeout)",
							NULL,
							&max_reconnect_attempts,
							60, 0, INT_MAX,
							PGC_USERSET,
							0,
							NULL, NULL, NULL);
	DefineCustomIntVariable("yb_amp.readahead_buffer_size",
							"number of prefetches to buffer",
							"This buffer is used to hold and manage prefetched "
							"data; so it is important that this buffer is at "
							"least as large as the configured value of all "
							"tablespaces' effective_io_concurrency and "
							"maintenance_io_concurrency, and your sessions' "
							"values for these settings.",
							&readahead_buffer_size,
							128, 16, 1024,
							PGC_USERSET,
							0,	/* no flags required */
							NULL, (GucIntAssignHook) &readahead_buffer_resize, NULL);
	DefineCustomIntVariable("yb_amp.readahead_getpage_pull_timeout",
							"readahead response pull timeout",
							"Time between active tries to pull data from the "
							"PageStream connection when we have pages which "
							"were read ahead but not yet received.",
							&readahead_getpage_pull_timeout_ms,
							50, 0, 5 * 60 * 1000,
							PGC_USERSET,
							GUC_UNIT_MS,
							NULL, NULL, NULL);
	DefineCustomIntVariable("yb_amp.protocol_version",
							"Version of compute<->page server protocol",
							NULL,
							&yb_amp_protocol_version,
							3,	/* use protocol version 3 */
							2,	/* min */
							3,	/* max */
							PGC_SU_BACKEND,
							0,	/* no flags required */
							NULL, NULL, NULL);

	DefineCustomIntVariable("yb_amp.pageserver_response_log_timeout",
							"pageserver response log timeout",
							"If the pageserver doesn't respond to a request within this timeout, "
							"a message is printed to the log.",
							&pageserver_response_log_timeout,
							10000, 100, INT_MAX,
							PGC_SUSET,
							GUC_UNIT_MS,
							NULL, NULL, NULL);

	DefineCustomIntVariable("yb_amp.pageserver_response_disconnect_timeout",
							"pageserver response diconnect timeout",
							"If the pageserver doesn't respond to a request within this timeout, "
							"disconnect and reconnect.",
							&pageserver_response_disconnect_timeout,
							30000, 100, INT_MAX,
							PGC_SUSET,
							GUC_UNIT_MS,
							NULL, NULL, NULL);

	DefineCustomEnumVariable(
							"yb_amp.compute_mode",
							"The compute endpoint node type",
							NULL,
							&yb_amp_compute_mode,
							CP_MODE_PRIMARY,
							yb_amp_compute_modes,
							PGC_POSTMASTER,
							0,
							NULL, NULL, NULL);

	yb_amp_log(PageStoreTrace, "libpagestore loaded");

	/*
	 * Retrieve the auth token to use when connecting to pageserver and
	 * safekeepers
	 */
	yb_amp_auth_token = getenv("YBAMP_AUTH_TOKEN");
	if (yb_amp_auth_token)
		yb_amp_log(LOG, "using storage auth token from YBAMP_AUTH_TOKEN environment variable");

	if (pageserver_connstring[0])
	{
		yb_amp_log(PageStoreTrace, "set yb_amp_smgr hook");
#if PG_MAJORVERSION_NUM >= 18
		smgr_register_yb_amp();
#else
		smgr_hook = smgr_yb_amp;
		smgr_init_hook = smgr_init_yb_amp;
#endif
		dbsize_hook = yb_amp_dbsize;
		read_slru_segment_hook = yb_amp_read_slru_segment;
	}

	memset(&page_server, 0, sizeof(page_server));
}
