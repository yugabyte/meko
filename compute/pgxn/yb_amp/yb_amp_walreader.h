#ifndef __YBAMP_WALREADER_H__
#define __YBAMP_WALREADER_H__

#include "access/xlogdefs.h"

/* forward declare so we don't have to expose the struct to the public */
struct YbAmpWALReader;
typedef struct YbAmpWALReader YbAmpWALReader;

/* avoid including walproposer.h as it includes us */
struct WalProposer;
typedef struct WalProposer WalProposer;

/* YbAmpWALRead return value */
typedef enum
{
	YBAMP_WALREAD_SUCCESS,
	YBAMP_WALREAD_WOULDBLOCK,
	YBAMP_WALREAD_ERROR,
} YbAmpWALReadResult;

extern YbAmpWALReader *YbAmpWALReaderAllocate(int wal_segment_size, XLogRecPtr available_lsn, char *log_prefix, TimeLineID tlid);
extern void YbAmpWALReaderFree(YbAmpWALReader *state);
extern void YbAmpWALReaderResetRemote(YbAmpWALReader *state);
extern TimeLineID YbAmpWALReaderLocalActiveTimeLineID(YbAmpWALReader *state);
extern YbAmpWALReadResult YbAmpWALRead(YbAmpWALReader *state, char *buf, XLogRecPtr startptr, Size count, TimeLineID tli);
extern pgsocket YbAmpWALReaderSocket(YbAmpWALReader *state);
extern uint32 YbAmpWALReaderEvents(YbAmpWALReader *state);
extern bool YbAmpWALReaderIsRemConnEstablished(YbAmpWALReader *state);
extern char *YbAmpWALReaderErrMsg(YbAmpWALReader *state);
extern XLogRecPtr YbAmpWALReaderGetRemLsn(YbAmpWALReader *state);
extern const WALOpenSegment *YbAmpWALReaderGetSegment(YbAmpWALReader *state);
extern bool yb_amp_wal_segment_open(YbAmpWALReader *state, XLogSegNo nextSegNo, TimeLineID *tli_p);
extern void yb_amp_wal_segment_close(YbAmpWALReader *state);
extern bool YbAmpWALReaderUpdateDonor(YbAmpWALReader *state);


#endif							/* __YBAMP_WALREADER_H__ */
