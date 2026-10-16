/* Minimal, bounded lifecycle observation. Request bodies are scanned/skipped,
 * never decoded, duplicated, or retained. Hooks perform no socket or disk I/O. */
#define _GNU_SOURCE
#include "events.h"
#include <ctype.h>
#include <pthread.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <time.h>
#define EVENT_CAP 4096
#define EVENT_BYTES (4u<<20)
#define EVENT_MAX 2048
#define EVENT_BATCH 128
typedef struct {
    const char *data;
    size_t size;
}
Slice;
typedef struct {
    char *json;
    size_t size;
    uint64_t sequence;
}
Entry;
static Entry ring[EVENT_CAP];
static size_t head,count,bytes;
static uint64_t next_sequence=1,acknowledged,loss_epoch;
static pthread_mutex_t mu=PTHREAD_MUTEX_INITIALIZER;
static J *field(J *j,const char *name) {
    J*v=NULL;
    if(j&&json_object_get_type(j)==4)json_object_object_get_ex(j,name,&v);
    return v;
}
static J *failure(const char *code) {
    J*j=json_object_new_object(),*e=json_object_new_object();
    json_object_object_add(e,"code",json_object_new_string(code));
    json_object_object_add(j,"ok",json_object_new_boolean(0));
    json_object_object_add(j,"error",e);
    return j;
}
static J *success(J *r) {
    J*j=json_object_new_object();
    json_object_object_add(j,"ok",json_object_new_boolean(1));
    json_object_object_add(j,"result",r);
    return j;
}
static void drop_head(void) {
    free(ring[head].json);
    bytes-=ring[head].size;
    ring[head]=(Entry) {
        0
    };
    head=(head+1)%EVENT_CAP;
    count--;
}
void cwr_events_clear(void) {
    pthread_mutex_lock(&mu);
    while(count)drop_head();
    pthread_mutex_unlock(&mu);
}
void cwr_events_reset(void) {
    pthread_mutex_lock(&mu);
    while(count)drop_head();
    head=bytes=count=0;
    next_sequence=1;
    acknowledged=loss_epoch=0;
    pthread_mutex_unlock(&mu);
}
static void lost(void) {
    pthread_mutex_lock(&mu);
    loss_epoch++;
    pthread_mutex_unlock(&mu);
}
static void whitespace(Slice s,size_t *p) {
    while(*p<s.size&&isspace((unsigned char)s.data[*p]))(*p)++;
}
static int skip_string(Slice s,size_t *p) {
    if(*p>=s.size||s.data[(*p)++]!='"')return 0;
    while(*p<s.size) {
        unsigned char ch=s.data[(*p)++];
        if(ch=='"')return 1;
        if(ch<32)return 0;
        if(ch=='\\') {
            if(*p>=s.size)return 0;
            (*p)++;
        }
    }
    return 0;
}
static int skip_value(Slice s,size_t *p,int depth) {
    whitespace(s,p);
    if(*p>=s.size||depth>64)return 0;
    char ch=s.data[*p];
    if(ch=='"')return skip_string(s,p);
    if(ch=='{'||ch=='[') {
        char close=ch=='{'?'}':']';
        (*p)++;
        whitespace(s,p);
        if(*p<s.size&&s.data[*p]==close) {
            (*p)++;
            return 1;
        }
        for(;;) {
            if(ch=='{') {
                if(!skip_string(s,p))return 0;
                whitespace(s,p);
                if(*p>=s.size||s.data[(*p)++]!=':')return 0;
            }
            if(!skip_value(s,p,depth+1))return 0;
            whitespace(s,p);
            if(*p>=s.size)return 0;
            if(s.data[*p]==close) {
                (*p)++;
                return 1;
            }
            if(s.data[(*p)++]!=',')return 0;
            whitespace(s,p);
        }
    }
    size_t start=*p;
    while(*p<s.size&&!isspace((unsigned char)s.data[*p])&&s.data[*p]!=','&&s.data[*p]!=']'&&s.data[*p]!='}')(*p)++;
    return *p>start;
}
static J *decode(Slice s) {
    if(!s.data||!s.size||s.size>2048)return NULL;
    char small[2060];
    memcpy(small,"{\"v\":",5);
    memcpy(small+5,s.data,s.size);
    small[s.size+5]='}';
    struct json_tokener*t=json_tokener_new_ex(8);
    if(!t)return NULL;
    json_tokener_set_flags(t,1|0x10);
    J*j=json_tokener_parse_ex(t,small,(int)s.size+6);
    J*v=NULL;
    if(json_tokener_get_error(t)==0&&j) {
        J*found=field(j,"v");
        if(found)v=json_object_get(found);
    }
    json_object_put(j);
    json_tokener_free(t);
    return v;
}
static int select_fields(Slice object,const char **names,Slice *out,size_t n) {
    size_t p=0;
    whitespace(object,&p);
    if(p>=object.size||object.data[p++]!='{')return 0;
    whitespace(object,&p);
    if(p<object.size&&object.data[p]=='}') {
        p++;
        whitespace(object,&p);
        return p==object.size;
    }
    for(;;) {
        size_t key_start=p;
        if(!skip_string(object,&p))return 0;
        Slice key= {
            object.data+key_start,p-key_start
        };
        whitespace(object,&p);
        if(p>=object.size||object.data[p++]!=':')return 0;
        whitespace(object,&p);
        size_t start=p;
        if(!skip_value(object,&p,0))return 0;
        if(key.size<=128) {
            J*k=decode(key);
            if(k&&json_object_get_type(k)==6) {
                const char*text=json_object_get_string(k);
                for(size_t i=0;i<n;i++)if(strlen(text)==(size_t)json_object_get_string_len(k)&&strcmp(text,names[i])==0) {
                    if(out[i].data) {
                        json_object_put(k);
                        return 0;
                    }
                    out[i]=(Slice) {
                        object.data+start,p-start
                    };
                }
            }
            json_object_put(k);
        }
        whitespace(object,&p);
        if(p>=object.size)return 0;
        if(object.data[p]=='}') {
            p++;
            whitespace(object,&p);
            return p==object.size;
        }
        if(object.data[p++]!=',')return 0;
        whitespace(object,&p);
    }
}
static const char *text(J*j) {
    if(!j||json_object_get_type(j)!=6)return NULL;
    const char*s=json_object_get_string(j);
    size_t n=strlen(s);
    return n>0&&n<=256&&n==(size_t)json_object_get_string_len(j)?s:NULL;
}
void cwr_observe_hook(const char *method,const void *payload,size_t size) {
    const char*kind=!strcmp(method,"request.intercept_before")?"started":!strcmp(method,"request.intercept_after")?"account_selected":"completed";
    Slice root[2]= {
        {
            0
        }
    },meta[4]= {
        {
            0
        }
    };
    const char*root_names[]= {
        "RequestID","Metadata"
    };
    const char*meta_names[]= {
        "caller_scope","selected_auth_id","source","generate"
    };
    if(!payload||!size||!select_fields((Slice) {
        payload,size
    },root_names,root,2)) {
        lost();
        return;
    }
    J*id=decode(root[0]);
    if(root[1].data&&!(root[1].size==4&&!memcmp(root[1].data,"null",4))&&!select_fields(root[1],meta_names,meta,4)) {
        json_object_put(id);
        lost();
        return;
    }
    J*identity=decode(meta[0]),*account=decode(meta[1]),*source=decode(meta[2]),*generate=decode(meta[3]);
    const char*request=text(id),*who=text(identity),*selected=text(account),*origin=text(source);
    if((origin&&!strcmp(origin,"plugin_host_model_callback"))||(generate&&json_object_get_type(generate)==1&&!json_object_get_int(generate)))goto done;
    if(!request) {
        lost();
        goto done;
    }
    if(!strcmp(kind,"started")&&!who)goto done;
    J*event=json_object_new_object();
    if(!event) {
        lost();
        goto done;
    }
    json_object_object_add(event,"kind",json_object_new_string(kind));
    json_object_object_add(event,"request_id",json_object_new_string(request));
    if(who)json_object_object_add(event,"identity",json_object_new_string(who));
    if(selected)json_object_object_add(event,"account_id",json_object_new_string(selected));
    struct timespec at;
    struct tm utc;
    char stamp[64],prefix[32];
    clock_gettime(CLOCK_REALTIME,&at);
    gmtime_r(&at.tv_sec,&utc);
    strftime(prefix,sizeof(prefix),"%Y-%m-%dT%H:%M:%S",&utc);
    snprintf(stamp,sizeof(stamp),"%s.%09ldZ",prefix,at.tv_nsec);
    json_object_object_add(event,"at",json_object_new_string(stamp));
    pthread_mutex_lock(&mu);
    json_object_object_add(event,"sequence",json_object_new_int64((int64_t)next_sequence));
    const char*raw=json_object_to_json_string_ext(event,0);
    size_t n=raw?strlen(raw):0;
    char*copy=n&&n<=EVENT_MAX?strdup(raw):NULL;
    if(copy) {
        while(count&&(count==EVENT_CAP||bytes+n>EVENT_BYTES)) {
            if(ring[head].sequence>acknowledged)loss_epoch++;
            drop_head();
        }
        ring[(head+count)%EVENT_CAP]=(Entry) {
            copy,n,next_sequence++
        };
        count++;
        bytes+=n;
    }
    else loss_epoch++;
    pthread_mutex_unlock(&mu);
    json_object_put(event);
    done:json_object_put(id);
    json_object_put(identity);
    json_object_put(account);
    json_object_put(source);
    json_object_put(generate);
}
J *cwr_events_pull(J*payload,const char*instance,const char*process) {
    J*value=field(payload,"after"),*limit_value=field(payload,"limit");
    if(!value||json_object_get_type(value)!=3||json_object_get_int64(value)<0)return failure("invalid_event_cursor");
    uint64_t after=(uint64_t)json_object_get_int64(value);
    int limit=limit_value?json_object_get_int(limit_value):EVENT_BATCH;
    if(limit<1||limit>EVENT_BATCH)return failure("invalid_event_cursor");
    J*r=json_object_new_object(),*list=json_object_new_array();
    pthread_mutex_lock(&mu);
    if(after>=next_sequence) {
        pthread_mutex_unlock(&mu);
        json_object_put(r);
        json_object_put(list);
        return failure("invalid_event_cursor");
    }
    uint64_t earliest=count?ring[head].sequence:next_sequence;
    json_object_object_add(r,"bridge_instance_id",json_object_new_string(instance));
    json_object_object_add(r,"host_process_id",json_object_new_string(process));
    json_object_object_add(r,"after_sequence",json_object_new_int64((int64_t)after));
    json_object_object_add(r,"earliest_sequence",json_object_new_int64((int64_t)earliest));
    json_object_object_add(r,"latest_sequence",json_object_new_int64((int64_t)next_sequence-1));
    json_object_object_add(r,"acknowledged_sequence",json_object_new_int64((int64_t)acknowledged));
    json_object_object_add(r,"loss_epoch",json_object_new_int64((int64_t)loss_epoch));
    json_object_object_add(r,"gap",json_object_new_boolean(after<acknowledged||after+1<earliest));
    for(size_t i=0,added=0;i<count&&added<(size_t)limit;i++) {
        Entry*e=&ring[(head+i)%EVENT_CAP];
        if(e->sequence<=after)continue;
        struct json_tokener*t=json_tokener_new_ex(8);
        J*event=t?json_tokener_parse_ex(t,e->json,(int)e->size):NULL;
        if(t)json_tokener_free(t);
        if(!event) {
            pthread_mutex_unlock(&mu);
            json_object_put(r);
            json_object_put(list);
            return failure("event_stream_failed");
        }
        json_object_array_add(list,event);
        added++;
    }
    pthread_mutex_unlock(&mu);
    json_object_object_add(r,"events",list);
    return success(r);
}
J *cwr_events_ack(J*payload) {
    J*v=field(payload,"through");
    if(!v||json_object_get_type(v)!=3||json_object_get_int64(v)<0)return failure("invalid_event_cursor");
    uint64_t through=(uint64_t)json_object_get_int64(v);
    pthread_mutex_lock(&mu);
    if(through>=next_sequence) {
        pthread_mutex_unlock(&mu);
        return failure("invalid_event_cursor");
    }
    if(through>acknowledged) {
        acknowledged=through;
        while(count&&ring[head].sequence<=through)drop_head();
    }
    pthread_mutex_unlock(&mu);
    return success(json_object_new_object());
}
