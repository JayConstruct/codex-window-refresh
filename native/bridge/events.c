/* Minimal, bounded lifecycle observation. Request bodies are scanned/skipped,
 * never decoded, duplicated, or retained. Hooks perform no socket or disk I/O. */
#define _GNU_SOURCE
#include "events.h"
#include <ctype.h>
#include <pthread.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <strings.h>
#include <time.h>
#define EVENT_CAP 4096
#define EVENT_BYTES (4u<<20)
#define EVENT_MAX 4096
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
static void clear_bindings(void);
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
    clear_bindings();
    pthread_mutex_unlock(&mu);
}
void cwr_events_reset(void) {
    pthread_mutex_lock(&mu);
    while(count)drop_head();
    clear_bindings();
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
    if(!s.data||!s.size||s.size>16384)return NULL;
    char small[16400];
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
static int select_fields_mode(Slice object,const char **names,Slice *out,size_t n,int insensitive) {
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
                for(size_t i=0;i<n;i++)if(strlen(text)==(size_t)json_object_get_string_len(k)&&(insensitive?strcasecmp(text,names[i]):strcmp(text,names[i]))==0) {
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
static int select_fields(Slice object,const char **names,Slice *out,size_t n) {
    return select_fields_mode(object,names,out,n,0);
}
static const char *text(J*j) {
    if(!j||json_object_get_type(j)!=6)return NULL;
    const char*s=json_object_get_string(j);
    size_t n=strlen(s);
    return n>0&&n<=256&&n==(size_t)json_object_get_string_len(j)?s:NULL;
}
#define BINDING_CAP 10000
#define RESPONSE_CAP 4096
typedef struct {
    char id[257],index[257],binding[257];
}
AccountBinding;
typedef struct {
    char request[257];
    AccountBinding account;
    time_t expires;
    int headers_seen;
}
ResponseBinding;
static AccountBinding *account_bindings;
static size_t account_count;
static time_t bindings_until;
static ResponseBinding responses[RESPONSE_CAP];
static time_t monotonic_seconds(void) {
    struct timespec at;
    clock_gettime(CLOCK_MONOTONIC,&at);
    return at.tv_sec;
}
static unsigned request_slot(const char *id) {
    uint32_t hash=2166136261u;
    for(const unsigned char*p=(const unsigned char*)id;*p;p++)hash=(hash^*p)*16777619u;
    return hash%RESPONSE_CAP;
}
static int compare_accounts(const void *a,const void *b) {
    return strcmp(((const AccountBinding*)a)->id,((const AccountBinding*)b)->id);
}
static AccountBinding *find_account(const char *id) {
    AccountBinding key= {
        0
    };
    if(!id||strlen(id)>256)return NULL;
    strcpy(key.id,id);
    return bsearch(&key,account_bindings,account_count,sizeof(key),compare_accounts);
}
static void clear_bindings(void) {
    free(account_bindings);
    account_bindings=NULL;
    account_count=0;
    bindings_until=0;
    memset(responses,0,sizeof(responses));
}
J *cwr_bindings_replace(J*payload) {
    J*list=field(payload,"accounts");
    if(!list||json_object_get_type(list)!=5||json_object_array_length(list)>BINDING_CAP)return failure("invalid_bindings");
    size_t n=json_object_array_length(list);
    AccountBinding*next=calloc(n?n:1,sizeof(*next));
    if(!next)return failure("allocation_failed");
    for(size_t i=0;i<n;i++) {
        J*item=json_object_array_get_idx(list,i);
        const char*id=text(field(item,"id")),*index=text(field(item,"auth_index")),*binding=text(field(item,"binding"));
        if(!id||!index||!binding) {
            free(next);
            return failure("invalid_bindings");
        }
        strcpy(next[i].id,id);
        strcpy(next[i].index,index);
        strcpy(next[i].binding,binding);
    }
    qsort(next,n,sizeof(*next),compare_accounts);
    for(size_t i=1;i<n;i++)if(!strcmp(next[i-1].id,next[i].id)) {
        free(next);
        return failure("invalid_bindings");
    }
    pthread_mutex_lock(&mu);
    free(account_bindings);
    account_bindings=next;
    account_count=n;
    bindings_until=monotonic_seconds()+90;
    for(size_t i=0;i<RESPONSE_CAP;i++)if(responses[i].request[0]) {
        AccountBinding*current=find_account(responses[i].account.id);
        if(!current||strcmp(current->index,responses[i].account.index)||strcmp(current->binding,responses[i].account.binding))memset(&responses[i],0,sizeof(responses[i]));
    }
    pthread_mutex_unlock(&mu);
    return success(json_object_new_object());
}
J *cwr_bindings_status(void) {
    J*j=json_object_new_object();
    pthread_mutex_lock(&mu);
    json_object_object_add(j,"accounts",json_object_new_int64((int64_t)account_count));
    json_object_object_add(j,"ready",json_object_new_boolean(bindings_until>monotonic_seconds()));
    pthread_mutex_unlock(&mu);
    return j;
}
static void enqueue(J*event) {
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
}
static const char*header_names[]= {
    "x-codex-allowed","x-codex-limit-reached", "x-codex-primary-used-percent","x-codex-primary-window-minutes","x-codex-primary-reset-at","x-codex-primary-reset-after-seconds", "x-codex-secondary-used-percent","x-codex-secondary-window-minutes","x-codex-secondary-reset-at","x-codex-secondary-reset-after-seconds"
};
static J *quota_headers(Slice object) {
    Slice values[10]= {
        {
            0
        }
    };
    if(!select_fields_mode(object,header_names,values,10,1))return NULL;
    J*headers=json_object_new_object();
    int fields=0;
    for(size_t i=0;i<10;i++)if(values[i].data) {
        J*value=decode(values[i]),*array=json_object_new_array();
        if(!value) {
            json_object_put(array);
            json_object_put(headers);
            return NULL;
        }
        size_t n=json_object_get_type(value)==5?json_object_array_length(value):1;
        if(n<1||n>4) {
            json_object_put(value);
            json_object_put(array);
            json_object_put(headers);
            return NULL;
        }
        for(size_t k=0;k<n;k++) {
            J*v=json_object_get_type(value)==5?json_object_array_get_idx(value,k):value;
            int type=json_object_get_type(v);
            const char*s=(type==1||type==2||type==3||type==6)?json_object_get_string(v):NULL;
            if(!s||!s[0]||strlen(s)>64||(type==6&&strlen(s)!=(size_t)json_object_get_string_len(v))) {
                json_object_put(value);
                json_object_put(array);
                json_object_put(headers);
                return NULL;
            }
            json_object_array_add(array,json_object_new_string(s));
        }
        json_object_put(value);
        json_object_object_add(headers,header_names[i],array);
        fields++;
    }
    if(!fields) {
        json_object_put(headers);
        return NULL;
    }
    return headers;
}
static J *quota_window(Slice object) {
    const char*names[]= {
        "limit_window_seconds","limitWindowSeconds","window_minutes","windowMinutes","used_percent","usedPercent","reset_at","resetAt","reset_after_seconds","resetAfterSeconds","reset_after"
    };
    Slice values[11]= {
        {
            0
        }
    };
    if(!select_fields(object,names,values,11))return NULL;
    J*window=json_object_new_object();
    for(size_t i=0;i<11;i++)if(values[i].data) {
        J*v=decode(values[i]);
        int type=json_object_get_type(v);
        if(!v||(type!=2&&type!=3)) {
            json_object_put(v);
            json_object_put(window);
            return NULL;
        }
        json_object_object_add(window,names[i],v);
    }
    return window;
}
static J *quota_rate(Slice object) {
    const char*names[]= {
        "allowed","limit_reached","limitReached","primary_window","primary","secondary_window","secondary"
    };
    Slice values[7]= {
        {
            0
        }
    };
    if(!select_fields(object,names,values,7))return NULL;
    J*rate=json_object_new_object();
    for(size_t i=0;i<7;i++)if(values[i].data) {
        J*v=i<3?decode(values[i]):quota_window(values[i]);
        if(!v||(i<3&&json_object_get_type(v)!=1)) {
            json_object_put(v);
            json_object_put(rate);
            return NULL;
        }
        json_object_object_add(rate,names[i],v);
    }
    return rate;
}
static int decode_base64(Slice value,char *out,size_t cap,size_t *size) {
    J*v=decode(value);
    const char*s=text(v);
    // Payload strings have a separate bounded allowance, unlike identifiers.
    if(v&&json_object_get_type(v)==6&&strlen(json_object_get_string(v))==(size_t)json_object_get_string_len(v))s=json_object_get_string(v);
    if(!s) {
        json_object_put(v);
        return 0;
    }
    size_t n=strlen(s),j=0;
    if(n%4||n/4*3>cap) {
        json_object_put(v);
        return 0;
    }
    const char*alphabet="ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/";
    for(size_t i=0;i<n;i+=4) {
        uint32_t bits=0;
        int pads=0;
        for(int k=0;k<4;k++) {
            const char*p=strchr(alphabet,s[i+k]);
            if(s[i+k]=='='&&k>=2&&i+4==n) {
                pads++;
                bits<<=6;
            }
            else if(!p||!s[i+k]||pads) {
                json_object_put(v);
                return 0;
            }
            else bits=(bits<<6)|(uint32_t)(p-alphabet);
        }
        out[j++]=(bits>>16)&255;
        if(pads<2)out[j++]=(bits>>8)&255;
        if(pads<1)out[j++]=bits&255;
    }
    json_object_put(v);
    *size=j;
    return 1;
}
static J *websocket_quota(Slice payload,const char*event_type,int *is_headers) {
    char decoded[12288];
    size_t n=0;
    if(!decode_base64(payload,decoded,sizeof(decoded),&n))return NULL;
    const char*names[]= {
        "type","headers","rate_limit","rate_limits","rateLimit"
    };
    Slice values[5]= {
        {
            0
        }
    };
    if(!select_fields((Slice) {
        decoded,n
    },names,values,5))return NULL;
    J*t=decode(values[0]);
    const char*type=text(t);
    if(!type||(event_type&&event_type[0]&&strcmp(type,event_type))) {
        json_object_put(t);
        return NULL;
    }
    J*result=NULL;
    if(!strcmp(type,"error")) {
        result=quota_headers(values[1]);
        *is_headers=1;
    }
    else if(!strcmp(type,"codex.rate_limits")) {
        result=json_object_new_object();
        int fields=0;
        for(size_t i=2;i<5;i++)if(values[i].data) {
            J*rate=quota_rate(values[i]);
            if(!rate) {
                json_object_put(result);
                result=NULL;
                break;
            }
            json_object_object_add(result,names[i],rate);
            fields++;
        }
        if(result&&!fields) {
            json_object_put(result);
            result=NULL;
        }
    }
    json_object_put(t);
    return result;
}
void cwr_observe_hook(const char *method,const void *payload,size_t size) {
    int before=!strcmp(method,"request.intercept_before"),selection=!strcmp(method,"request.intercept_after"),completion=!strcmp(method,"request.complete");
    int lifecycle=before||selection||completion,stream=!strcmp(method,"response.intercept_stream_chunk"),ws=!strcmp(method,"websocket.response_event");
    const char*root_names[]= {
        "RequestID","Metadata","ResponseHeaders","ChunkIndex","Provider","AuthID","EventType","Payload"
    };
    const char*meta_names[]= {
        "caller_scope","selected_auth_id","source","generate"
    };
    Slice root[8]= {
        {
            0
        }
    },meta[4]= {
        {
            0
        }
    };
    if(!payload||!size||!select_fields((Slice) {
        payload,size
    },root_names,root,8)) {
        if(lifecycle)lost();
        return;
    }
    if(root[1].data&&!(root[1].size==4&&!memcmp(root[1].data,"null",4))&&!select_fields(root[1],meta_names,meta,4)) {
        if(lifecycle)lost();
        return;
    }
    J*id=decode(root[0]),*identity=decode(meta[0]),*account=decode(meta[1]),*source=decode(meta[2]),*generate=decode(meta[3]);
    const char*request=text(id),*who=text(identity),*selected=text(account),*origin=text(source);
    int excluded=(origin&&!strcmp(origin,"plugin_host_model_callback"))||(generate&&json_object_get_type(generate)==1&&!json_object_get_int(generate));
    if(!request) {
        if(lifecycle)lost();
        goto done;
    }
    unsigned slot=request_slot(request);
    if(lifecycle) {
        pthread_mutex_lock(&mu);
        ResponseBinding*r=&responses[slot];
        if(before||selection) {
            memset(r,0,sizeof(*r));
            AccountBinding*a=selection&&!excluded&&bindings_until>monotonic_seconds()?find_account(selected):NULL;
            if(a) {
                strcpy(r->request,request);
                r->account=*a;
                r->expires=monotonic_seconds()+600;
            }
        }
        if(completion&&!strcmp(r->request,request))r->expires=monotonic_seconds()+120;
        pthread_mutex_unlock(&mu);
        if(excluded||(before&&!who))goto done;
        J*event=json_object_new_object();
        json_object_object_add(event,"kind",json_object_new_string(before?"started":selection?"account_selected":"completed"));
        json_object_object_add(event,"request_id",json_object_new_string(request));
        if(who)json_object_object_add(event,"identity",json_object_new_string(who));
        if(selected)json_object_object_add(event,"account_id",json_object_new_string(selected));
        enqueue(event);
        json_object_put(event);
        goto done;
    }
    if(excluded)goto done;
    if(stream) {
        J*index=decode(root[3]);
        int init=index&&json_object_get_type(index)==3&&json_object_get_int64(index)==-1;
        json_object_put(index);
        if(!init)goto done;
    }
    J*provider=decode(root[4]),*auth=decode(root[5]),*eventtype=decode(root[6]);
    const char*provider_name=text(provider),*auth_id=text(auth),*type=text(eventtype);
    J*evidence=NULL;
    int is_headers=1;
    if(ws) {
        if(!provider_name||strcmp(provider_name,"codex")||!auth_id||(type&&strcmp(type,"codex.rate_limits")&&strcmp(type,"error")))goto response_done;
        is_headers=0;
        evidence=websocket_quota(root[7],type,&is_headers);
    }
    else evidence=quota_headers(root[2]);
    if(!evidence)goto response_done;
    const char*bounded=json_object_to_json_string_ext(evidence,0);
    if(!bounded||strlen(bounded)>2048) {
        json_object_put(evidence);
        goto response_done;
    }
    AccountBinding captured= {
        0
    };
    int accept=0;
    pthread_mutex_lock(&mu);
    ResponseBinding*r=&responses[slot];
    if(bindings_until>monotonic_seconds()&&!strcmp(r->request,request)&&r->expires>monotonic_seconds()&&(!selected||!strcmp(selected,r->account.id))&&(!auth_id||!strcmp(auth_id,r->account.id))&&(ws||!r->headers_seen)) {
        captured=r->account;
        accept=1;
        if(!ws)r->headers_seen=1;
    }
    pthread_mutex_unlock(&mu);
    if(accept) {
        J*event=json_object_new_object();
        json_object_object_add(event,"kind",json_object_new_string("quota_observed"));
        json_object_object_add(event,"request_id",json_object_new_string(request));
        json_object_object_add(event,"account_id",json_object_new_string(captured.id));
        json_object_object_add(event,"auth_index",json_object_new_string(captured.index));
        json_object_object_add(event,"binding",json_object_new_string(captured.binding));
        json_object_object_add(event,"source",json_object_new_string(ws?"cpa_websocket_quota":"cpa_response_headers"));
        json_object_object_add(event,is_headers?"headers":"quota",evidence);
        evidence=NULL;
        enqueue(event);
        json_object_put(event);
    }
    json_object_put(evidence);
    response_done:json_object_put(provider);
    json_object_put(auth);
    json_object_put(eventtype);
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
