/*-------------------------------------------------------------------------
 *
 * yb_amp.h
 *	  Functions used in the initialization of this extension.
 *
 *-------------------------------------------------------------------------
 */

#ifndef YBAMP_H
#define YBAMP_H

#include "access/xlogdefs.h"
#include "utils/wait_event.h"

/* GUCs */
extern char *yb_amp_auth_token;
extern char *yb_amp_branch_id;
extern char *wal_acceptors_list;
extern int	wal_acceptor_reconnect_timeout;
extern int	wal_acceptor_connection_timeout;
extern int	readahead_getpage_pull_timeout_ms;
extern bool	disable_wal_prev_lsn_checks;
extern bool	lakebase_mode;

extern bool AmPrewarmWorker;

#if PG_MAJORVERSION_NUM >= 17
extern uint32		WAIT_EVENT_YBAMP_LFC_MAINTENANCE;
extern uint32		WAIT_EVENT_YBAMP_LFC_READ;
extern uint32		WAIT_EVENT_YBAMP_LFC_TRUNCATE;
extern uint32		WAIT_EVENT_YBAMP_LFC_WRITE;
extern uint32		WAIT_EVENT_YBAMP_LFC_CV_WAIT;
extern uint32		WAIT_EVENT_YBAMP_PS_STARTING;
extern uint32		WAIT_EVENT_YBAMP_PS_CONFIGURING;
extern uint32		WAIT_EVENT_YBAMP_PS_SEND;
extern uint32		WAIT_EVENT_YBAMP_PS_READ;
extern uint32		WAIT_EVENT_YBAMP_WAL_DL;
#else
#define WAIT_EVENT_YBAMP_LFC_MAINTENANCE	PG_WAIT_EXTENSION
#define WAIT_EVENT_YBAMP_LFC_READ		WAIT_EVENT_BUFFILE_READ
#define WAIT_EVENT_YBAMP_LFC_TRUNCATE	WAIT_EVENT_BUFFILE_TRUNCATE
#define WAIT_EVENT_YBAMP_LFC_WRITE		WAIT_EVENT_BUFFILE_WRITE
#define WAIT_EVENT_YBAMP_LFC_CV_WAIT 	WAIT_EVENT_BUFFILE_READ
#define WAIT_EVENT_YBAMP_PS_STARTING		PG_WAIT_EXTENSION
#define WAIT_EVENT_YBAMP_PS_CONFIGURING	PG_WAIT_EXTENSION
#define WAIT_EVENT_YBAMP_PS_SEND			PG_WAIT_EXTENSION
#define WAIT_EVENT_YBAMP_PS_READ			PG_WAIT_EXTENSION
#define WAIT_EVENT_YBAMP_WAL_DL			WAIT_EVENT_WAL_READ
#endif


#define YBAMP_TAG "[YBAMP_SMGR] "
#define yb_amp_log(tag, fmt, ...) ereport(tag,                                  \
										(errmsg(YBAMP_TAG fmt, ##__VA_ARGS__), \
										 errhidestmt(true), errhidecontext(true), errposition(0), internalerrposition(0)))


extern void pg_init_libpagestore(void);
extern void pg_init_walproposer(void);

extern uint64 BackpressureThrottlingTime(void);
extern void SetYbAmpCurrentClusterSize(uint64 size);
extern uint64 GetYbAmpCurrentClusterSize(void);
extern void replication_feedback_get_lsns(XLogRecPtr *writeLsn, XLogRecPtr *flushLsn, XLogRecPtr *applyLsn);

extern PGDLLEXPORT void WalProposerSync(int argc, char *argv[]);
extern PGDLLEXPORT void WalProposerMain(Datum main_arg);
extern PGDLLEXPORT void LogicalSlotsMonitorMain(Datum main_arg);

extern void LfcShmemRequest(void);
extern void RelsizeCacheShmemRequest(void);
extern void WalproposerShmemRequest(void);
extern void LwLsnCacheShmemRequest(void);
extern void YbAmpPerfCountersShmemRequest(void);

extern void LfcShmemInit(void);
extern void RelsizeCacheShmemInit(void);
extern void WalproposerShmemInit(void);
extern void LwLsnCacheShmemInit(void);
extern void YbAmpPerfCountersShmemInit(void);


#endif							/* YBAMP_H */
