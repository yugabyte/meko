#ifndef YBAMP_RMGR_H
#define YBAMP_RMGR_H
#if PG_MAJORVERSION_NUM >= 16
#include "access/xlog_internal.h"
#include "replication/decode.h"
#include "replication/logical.h"

extern void yb_amp_rm_desc(StringInfo buf, XLogReaderState *record);
extern void yb_amp_rm_decode(LogicalDecodingContext *ctx, XLogRecordBuffer *buf);
extern const char *yb_amp_rm_identify(uint8 info);

#endif
#endif //YBAMP_RMGR_H
