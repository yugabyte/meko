/*-------------------------------------------------------------------------
 *
 * pagestore_client.h
 *
 *
 * Portions Copyright (c) 1996-2021, PostgreSQL Global Development Group
 * Portions Copyright (c) 1994, Regents of the University of California
 *
 *-------------------------------------------------------------------------
 */
#ifndef PAGESTORE_CLIENT_h
#define PAGESTORE_CLIENT_h

#include "yb_amp_pgversioncompat.h"

#include "access/slru.h"
#include "access/xlogdefs.h"
#include RELFILEINFO_HDR
#include "lib/stringinfo.h"
#include "storage/block.h"
#include "storage/buf_internals.h"

#define MAX_PAGESERVER_CONNSTRING_SIZE 256

typedef enum
{
	/* pagestore_client -> pagestore */
	T_YbAmpExistsRequest = 0,
	T_YbAmpNblocksRequest,
	T_YbAmpGetPageRequest,
	T_YbAmpDbSizeRequest,
	T_YbAmpGetSlruSegmentRequest,
	/* future tags above this line */
	T_YbAmpTestRequest = 99, /* only in cfg(feature = "testing") */

	/* pagestore -> pagestore_client */
	T_YbAmpExistsResponse = 100,
	T_YbAmpNblocksResponse,
	T_YbAmpGetPageResponse,
	T_YbAmpErrorResponse,
	T_YbAmpDbSizeResponse,
	T_YbAmpGetSlruSegmentResponse,
	T_YbAmpGoingAwayResponse, /* unsolicited; pageserver closing the connection */
	/* future tags above this line */
	T_YbAmpTestResponse = 199, /* only in cfg(feature = "testing") */
} YbAmpMessageTag;

typedef uint64 YbAmpRequestId;

/* base struct for c-style inheritance */
typedef struct
{
	YbAmpMessageTag tag;
	YbAmpRequestId reqid;
	XLogRecPtr	lsn;
	XLogRecPtr	not_modified_since;
} YbAmpMessage;

#define messageTag(m) (((const YbAmpMessage *)(m))->tag)

/* SLRUs downloadable from page server */
typedef enum {
	SLRU_CLOG,
	SLRU_MULTIXACT_MEMBERS,
	SLRU_MULTIXACT_OFFSETS
} SlruKind;

/*--
 * supertype of all the YbAmp*Request structs below.
 *
 * All requests contain two LSNs:
 *
 * lsn:                request page (or relation size, etc) at this LSN
 * not_modified_since: Hint that the page hasn't been modified between
 *                     this LSN and the request LSN (`lsn`).
 *
 * To request the latest version of a page, you can use MAX_LSN as the request
 * LSN.
 *
 * If you don't know any better, you can always set 'not_modified_since' equal
 * to 'lsn', but providing a lower value can speed up processing the request
 * in the pageserver, as it doesn't need to wait for the WAL to arrive, and it
 * can skip traversing through recent layers which we know to not contain any
 * versions for the requested page.
 *
 * These structs describe the V2 of these requests. (The old now-defunct V1
 * protocol contained just one LSN and a boolean 'latest' flag.)
 *
 * V3 version of protocol adds request ID to all requests. This request ID is also included in response
 * as well as other fields from requests, which allows to verify that we receive response for our request.
 * We copy fields from request to response to make checking more reliable: request ID is formed from process ID
 * and local counter, so in principle there can be duplicated requests IDs if process PID is reused.
 */
typedef YbAmpMessage YbAmpRequest;

typedef struct
{
	YbAmpRequest hdr;
	NRelFileInfo rinfo;
	ForkNumber	forknum;
} YbAmpExistsRequest;

typedef struct
{
	YbAmpRequest hdr;
	NRelFileInfo rinfo;
	ForkNumber	forknum;
} YbAmpNblocksRequest;

typedef struct
{
	YbAmpRequest hdr;
	Oid			dbNode;
} YbAmpDbSizeRequest;

typedef struct
{
	YbAmpRequest hdr;
	NRelFileInfo rinfo;
	ForkNumber	forknum;
	BlockNumber blkno;
} YbAmpGetPageRequest;

typedef struct
{
	YbAmpRequest hdr;
	SlruKind	kind;
	int			segno;
} YbAmpGetSlruSegmentRequest;


/* supertype of all the YbAmp*Response structs below */
typedef YbAmpMessage YbAmpResponse;

typedef struct
{
	YbAmpExistsRequest req;
	bool		exists;
} YbAmpExistsResponse;

typedef struct
{
	YbAmpNblocksRequest req;
	uint32		n_blocks;
} YbAmpNblocksResponse;

typedef struct
{
	YbAmpGetPageRequest req;
	char		page[FLEXIBLE_ARRAY_MEMBER];
} YbAmpGetPageResponse;

#define PS_GETPAGERESPONSE_SIZE (MAXALIGN(offsetof(YbAmpGetPageResponse, page) + BLCKSZ))

typedef struct
{
	YbAmpDbSizeRequest req;
	int64		db_size;
} YbAmpDbSizeResponse;

typedef struct
{
	YbAmpResponse req;
	char		message[FLEXIBLE_ARRAY_MEMBER]; /* null-terminated error
												 * message */
} YbAmpErrorResponse;

typedef struct
{
	YbAmpGetSlruSegmentRequest req;
	int			n_blocks;
	char		data[BLCKSZ * SLRU_PAGES_PER_SEGMENT];
} YbAmpGetSlruSegmentResponse;

/*
 * Unsolicited "I'm shutting down" frame the pageserver sends just before
 * closing the connection on graceful shutdown. Receipt is the compute's
 * cue to abandon in-flight requests and reconnect now (vs waiting out
 * the pageserver_response_disconnect_timeout safety net).
 *
 * `req` is zero-filled padding so the V3 echo-header parser stays
 * symmetric — there's no request being echoed.
 */
typedef enum
{
	YBAMP_GOING_AWAY_SHUTDOWN = 0
} YbAmpGoingAwayReason;

typedef struct
{
	YbAmpResponse req;
	uint8		reason;	/* YbAmpGoingAwayReason */
} YbAmpGoingAwayResponse;


extern StringInfoData nm_pack_request(YbAmpRequest *msg);
extern YbAmpResponse *nm_unpack_response(StringInfo s);
extern char *nm_to_string(YbAmpMessage *msg);

/*
 * If debug_compare_local>DEBUG_COMPARE_LOCAL_NONE, we pass through all the SMGR API
 * calls to md.c, and *also* do the calls to the Page Server. On every
 * read, compare the versions we read from local disk and Page Server,
 * and Assert that they are identical.
 */
typedef enum
{
	DEBUG_COMPARE_LOCAL_NONE,     /* normal mode - pages are storted locally only for unlogged relations */
	DEBUG_COMPARE_LOCAL_PREFETCH, /* if page is found in prefetch ring, then compare it with local and return */
	DEBUG_COMPARE_LOCAL_LFC,      /* if page is found in LFC or prefetch ring, then compare it with local and return */
	DEBUG_COMPARE_LOCAL_ALL       /* always fetch page from PS and compare it with local */
} DebugCompareLocalMode;

extern int debug_compare_local;

/*
 * Page server client API. There is exactly one page server per endpoint.
 */

/*
 * Send a request to the page server. This assigns a request_id to the request,
 * which the caller can extract from the request struct.
 */
extern bool pageserver_send(YbAmpRequest *request);
/*
 * Blocking read for the next response.
 *
 * When a CANCEL signal is handled, the connection state will be unmodified.
 */
extern YbAmpResponse *pageserver_receive(void);
/*
 * Try to get the next response from the TCP buffers, if any. Returns NULL when
 * the data is not yet available.
 *
 * This will raise errors only for malformed responses (we can't put them back
 * into the connection). All other error conditions are soft errors and return
 * NULL as "no response available".
 */
extern YbAmpResponse *pageserver_try_receive(void);
/*
 * Make sure all requests are sent to the page server.
 */
extern bool pageserver_flush(void);
/*
 * Disconnect from the page server.
 */
extern void pageserver_disconnect_conn(void);

extern void prefetch_on_ps_disconnect(void);

extern char *pageserver_connstring;
extern int	flush_every_n_requests;
extern int	readahead_buffer_size;
extern char *yb_amp_branch_id;
extern int32 max_cluster_size;
extern int  yb_amp_protocol_version;

extern const f_smgr *smgr_yb_amp(ProcNumber backend, NRelFileInfo rinfo);
extern bool yb_amp_read_slru_segment(const char *path, int segno);
extern void smgr_init_yb_amp(void);
extern void readahead_buffer_resize(int newsize, void *extra);


/*
 * LSN values associated with each request to the pageserver
 */
typedef struct
{
	/*
	 * 'request_lsn' is the main value that determines which page version to
	 * fetch.
	 */
	XLogRecPtr request_lsn;

	/*
	 * A hint to the pageserver that the requested page hasn't been modified
	 * between this LSN and 'request_lsn'. That allows the pageserver to
	 * return the page faster, without waiting for 'request_lsn' to arrive in
	 * the pageserver, as long as 'not_modified_since' has arrived.
	 */
	XLogRecPtr not_modified_since;

	/*
	 * 'effective_request_lsn' is not included in the request that's sent to
	 * the pageserver, but is used to keep track of the latest LSN of when the
	 * request was made. In a standby server, this is always the same as the
	 * 'request_lsn', but in the primary we use UINT64_MAX as the
	 * 'request_lsn' to request the latest page version, so we need this
	 * separate field to remember that latest LSN was when the request was
	 * made. It's needed to manage prefetch request, to verify if the response
	 * to a prefetched request is still valid.
	 */
	XLogRecPtr effective_request_lsn;
} yb_amp_request_lsns;

extern PGDLLEXPORT void yb_amp_read_at_lsn(NRelFileInfo rnode, ForkNumber forkNum, BlockNumber blkno,
										 yb_amp_request_lsns request_lsns, void *buffer);
extern int64 yb_amp_dbsize(Oid dbNode);

extern void yb_amp_get_request_lsns(NRelFileInfo rinfo, ForkNumber forknum,
								  BlockNumber blkno, yb_amp_request_lsns *output,
								  BlockNumber nblocks);

/* utils for yb_amp relsize cache */
extern void relsize_hash_init(void);
extern bool get_cached_relsize(NRelFileInfo rinfo, ForkNumber forknum, BlockNumber *size);
extern void set_cached_relsize(NRelFileInfo rinfo, ForkNumber forknum, BlockNumber size);
extern void update_cached_relsize(NRelFileInfo rinfo, ForkNumber forknum, BlockNumber size);
extern void forget_cached_relsize(NRelFileInfo rinfo, ForkNumber forknum);

#endif							/* PAGESTORE_CLIENT_H */
