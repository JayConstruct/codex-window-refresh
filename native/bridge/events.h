#ifndef CWR_EVENTS_H
#define CWR_EVENTS_H
#include "json_api.h"
void cwr_events_reset(void);
void cwr_events_clear(void);
void cwr_observe_hook(const char *,const void *,size_t);
J *cwr_events_pull(J *,const char *,const char *);
J *cwr_events_ack(J *);
J *cwr_bindings_replace(J *);
J *cwr_bindings_status(void);
#endif
