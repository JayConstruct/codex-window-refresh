/* CPA ABI 1 / schema 6 bridge. No rules, credentials on disk, or Go runtime.
 * Host callbacks permit local auth reads and explicitly pinned manual streams.
 * The independent daemon performs HTTP and owns all durable business state. */
#define _GNU_SOURCE
#include "json_api.h"
#include "events.h"
#include "version.h"
#include <arpa/inet.h>
#include <curl/curl.h>
#include <errno.h>
#include <fcntl.h>
#include <limits.h>
#include <pthread.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/file.h>
#include <sys/random.h>
#include <sys/socket.h>
#include <sys/stat.h>
#include <sys/un.h>
#include <time.h>
#include <unistd.h>
#define FRAME_LIMIT (4u<<20)
#define HTTP_BODY_LIMIT (2u<<20)
#define WORKERS 10
#define HOST_CALLS 8
#define MODEL_LEASE_SECONDS 120
#define QUEUE 32
#define MGMT "/v0/management/codex-window-refresh/v2/"
#define RESOURCE "/v0/resource/plugins/codex-window-refresh/"
typedef struct {
    void *ptr;
    size_t len;
}
Buffer;
typedef int (*HostCall)(void *,const char *,const uint8_t *,size_t,Buffer *);
typedef void (*HostFree)(void *,size_t);
typedef struct {
    uint32_t abi_version;
    void *host_ctx;
    HostCall call;
    HostFree free_buffer;
}
HostAPI;
typedef struct {
    uint32_t abi_version;
    int(*call)(char *,uint8_t *,size_t,Buffer *);
    HostFree free_buffer;
    void(*shutdown)(void);
}
PluginAPI;
static HostAPI host;
static pthread_rwlock_t gate=PTHREAD_RWLOCK_INITIALIZER;
static pthread_mutex_t queue_mu=PTHREAD_MUTEX_INITIALIZER;
static pthread_cond_t available=PTHREAD_COND_INITIALIZER,drained=PTHREAD_COND_INITIALIZER;
static pthread_t accept_thread,workers[WORKERS];
static int listener=-1,owner_lock=-1,stopping=1,running=0,live=0,queued[QUEUE],qhead=0,qsize=0,created=0,initialized=0;
static int host_active;
static pthread_cond_t streams_changed=PTHREAD_COND_INITIALIZER;
static struct {
    char id[257];
    int used,reading,closing,closed;
    time_t touched,retry_at;
} streams[HOST_CALLS];
static char bridge_path[sizeof(((struct sockaddr_un*)0)->sun_path)],daemon_path[sizeof(bridge_path)],instance[65];
static char process_identity[128];
static int identify_process(void) {
    char boot[64],stat[4096];
    FILE *f=fopen("/proc/sys/kernel/random/boot_id","r");
    if(!f)return 0;
    int valid=fgets(boot,sizeof(boot),f)!=NULL;
    fclose(f);
    if(!valid)return 0;
    boot[strcspn(boot,"\r\n")]=0;
    f=fopen("/proc/self/stat","r");
    if(!f)return 0;
    valid=fgets(stat,sizeof(stat),f)!=NULL;
    fclose(f);
    if(!valid)return 0;
    char*p=strrchr(stat,')');
    if(!p)return 0;
    p++;
    for(int field=3;field<22;field++) {
        while(*p==' ')p++;
        while(*p&&*p!=' ')p++;
        if(!*p)return 0;
    }
    while(*p==' ')p++;
    char*ticks=p;
    while(*p>='0'&&*p<='9')p++;
    if(p==ticks||p-ticks>24)return 0;
    *p=0;
    return snprintf(process_identity,sizeof(process_identity),"%s:%ld:%s",boot,(long)getpid(),ticks)<(int)sizeof(process_identity);
}
static J *field(J *j,const char *key) {
    if(!j||json_object_get_type(j)!=4)return NULL;
    J *v=NULL;
    json_object_object_get_ex(j,key,&v);
    return v;
}
static const char *string(J *v) {
    if(!v||json_object_get_type(v)!=6)return NULL;
    const char*s=json_object_get_string(v);
    return strlen(s)==(size_t)json_object_get_string_len(v)?s:NULL;
}
static J *parse(const void *raw,size_t size) {
    if(!raw||!size||size>FRAME_LIMIT)return NULL;
    struct json_tokener*t=json_tokener_new_ex(32);
    if(!t)return NULL;
    json_tokener_set_flags(t,1|0x10);
    J*j=json_tokener_parse_ex(t,raw,(int)size);
    size_t end=json_tokener_get_parse_end(t);
    int ok=json_tokener_get_error(t)==0&&j&&json_object_get_type(j)==4;
    while(end<size&&(((const char*)raw)[end]==' '||((const char*)raw)[end]=='\r'||((const char*)raw)[end]=='\n'||((const char*)raw)[end]=='\t'))end++;
    json_tokener_free(t);
    if(!ok||end!=size) {
        json_object_put(j);
        return NULL;
    }
    return j;
}
static J *error(const char *code) {
    J*j=json_object_new_object(),*e=json_object_new_object();
    json_object_object_add(e,"code",json_object_new_string(code));
    json_object_object_add(j,"ok",json_object_new_boolean(0));
    json_object_object_add(j,"error",e);
    return j;
}
static J *ok(J *result) {
    J*j=json_object_new_object();
    json_object_object_add(j,"ok",json_object_new_boolean(1));
    json_object_object_add(j,"result",result);
    return j;
}
static int write_buffer(Buffer*out,J*j) {
    const char*raw=json_object_to_json_string_ext(j,0);
    if(!raw)return 1;
    size_t n=strlen(raw);
    if(n>FRAME_LIMIT)return 1;
    out->ptr=malloc(n);
    if(!out->ptr)return 1;
    memcpy(out->ptr,raw,n);
    out->len=n;
    return 0;
}
static void release(void *ptr,size_t len) {
    (void)len;
    free(ptr);
}
static int transfer(int fd,void*data,size_t size,int writing) {
    char*p=data;
    while(size) {
        ssize_t n=writing?send(fd,p,size,MSG_NOSIGNAL):recv(fd,p,size,0);
        if(n<0&&errno==EINTR)continue;
        if(n<=0)return 0;
        p+=n;
        size-=(size_t)n;
    }
    return 1;
}
static void respond(int fd,J*j) {
    const char*raw=json_object_to_json_string_ext(j,0);
    size_t n=strlen(raw);
    if(n&&n<=FRAME_LIMIT) {
        uint32_t size=htonl((uint32_t)n);
        if(transfer(fd,&size,4,1))transfer(fd,(void*)raw,n,1);
    }
    json_object_put(j);
}
static int peer(int fd) {
    struct ucred cred;
    socklen_t n=sizeof(cred);
    return getsockopt(fd,SOL_SOCKET,SO_PEERCRED,&cred,&n)==0&&cred.uid==geteuid();
}
static int bounded_text(J*value,size_t limit) {
    const char*text=string(value);
    if(!text||!text[0]||strlen(text)>limit)return 0;
    int visible=0;
    for(const unsigned char*next=(const unsigned char*)text;*next;next++) {
        if(*next<32||*next==127)return 0;
        if(*next!=' ')visible=1;
    }
    return visible;
}
static int equals(J*value,const char*expected) {
    const char*text=string(value);
    return text&&!strcmp(text,expected);
}
static int model_payload(J*payload) {
    const char*body=string(field(payload,"body"));
    const char*model=string(field(payload,"model"));
    if(!equals(field(payload,"forced_provider"),"codex")||!equals(field(payload,"entry_protocol"),"openai-response")||!equals(field(payload,"exit_protocol"),"codex")||!bounded_text(field(payload,"auth_id"),256)||!bounded_text(field(payload,"model"),200)||json_object_get_type(field(payload,"stream"))!=1||!json_object_get_int(field(payload,"stream"))||!body)return 0;
    if(strpbrk(model," \t\n\r\v\f"))return 0;
    size_t length=strlen(body);
    if(!length||length>65536||length%4)return 0;
    int padding=0;
    for(size_t index=0;index<length;index++) {
        unsigned char next=(unsigned char)body[index];
        if(next=='='&&index>=length-2)padding++;
        else if(padding||!((next>='A'&&next<='Z')||(next>='a'&&next<='z')||(next>='0'&&next<='9')||next=='+'||next=='/'))return 0;
    }
    return padding<=2;
}
static J *host_callback(const char*method,J*payload) {
    const char*raw=json_object_to_json_string_ext(payload,0);
    Buffer result={0};
    int status=host.call(host.host_ctx,method,(const uint8_t*)raw,strlen(raw),&result);
    J*response=NULL;
    if(!status&&result.ptr&&result.len<=FRAME_LIMIT)response=parse(result.ptr,result.len);
    if(result.ptr)host.free_buffer(result.ptr,result.len);
    J*success=field(response,"ok");
    if(!success||json_object_get_type(success)!=1||!json_object_get_int(success)) {
        json_object_put(response);
        return error("host_callback_failed");
    }
    return response;
}
static void *stream_reaper(void*arg) {
    (void)arg;
    pthread_mutex_lock(&queue_mu);
    for(;;) {
        int slot=-1,pending=0;
        struct timespec now;
        clock_gettime(CLOCK_MONOTONIC,&now);
        for(int index=0;index<HOST_CALLS;index++) {
            if(!streams[index].used)continue;
            pending=1;
            if(streams[index].id[0]&&!streams[index].closing&&now.tv_sec>=streams[index].retry_at&&(stopping||streams[index].retry_at||now.tv_sec-streams[index].touched>=MODEL_LEASE_SECONDS)&&host_active<HOST_CALLS) {
                slot=index;
                break;
            }
        }
        if(slot>=0) {
            char id[257];
            strcpy(id,streams[slot].id);
            streams[slot].closing=1;
            host_active++;
            pthread_mutex_unlock(&queue_mu);
            J*payload=json_object_new_object();
            json_object_object_add(payload,"stream_id",json_object_new_string(id));
            J*response=host_callback("host.model.stream_close",payload);
            int released=json_object_get_int(field(response,"ok"));
            json_object_put(response);
            json_object_put(payload);
            pthread_mutex_lock(&queue_mu);
            host_active--;
            if(released) {
                streams[slot].closed=1;
                if(!streams[slot].reading)memset(&streams[slot],0,sizeof(streams[slot]));
            }
            else {
                clock_gettime(CLOCK_MONOTONIC,&now);
                streams[slot].closing=0;
                streams[slot].retry_at=now.tv_sec+1;
            }
            continue;
        }
        if(stopping&&!pending&&!host_active) {
            live--;
            pthread_cond_broadcast(&drained);
            pthread_mutex_unlock(&queue_mu);
            return NULL;
        }
        struct timespec deadline;
        clock_gettime(CLOCK_REALTIME,&deadline);
        deadline.tv_sec++;
        pthread_cond_timedwait(&streams_changed,&queue_mu,&deadline);
    }
}
static J *rpc(J*req) {
    const char*method=string(field(req,"method"));
    if(json_object_get_int(field(req,"protocol_version"))!=1||!method)return error("bridge_protocol_incompatible");
    if(strcmp(method,"bridge.hello")==0) {
        J*j=json_object_new_object();
        json_object_object_add(j,"protocol_version",json_object_new_int(1));
        json_object_object_add(j,"bridge_instance_id",json_object_new_string(instance));
        json_object_object_add(j,"read_only",json_object_new_boolean(1));
        json_object_object_add(j,"prompt_generation_supported",json_object_new_boolean(1));
        json_object_object_add(j,"version",json_object_new_string(CWR_VERSION));
        json_object_object_add(j,"max_host_calls",json_object_new_int(HOST_CALLS));
        pthread_mutex_lock(&queue_mu);
        json_object_object_add(j,"active_host_calls",json_object_new_int(host_active));
        pthread_mutex_unlock(&queue_mu);
        json_object_object_add(j,"events_supported",json_object_new_boolean(1));
        json_object_object_add(j,"passive_quota_supported",json_object_new_boolean(1));
        json_object_object_add(j,"quota_observer",cwr_bindings_status());
        json_object_object_add(j,"host_process_id",json_object_new_string(process_identity));
        return ok(j);
    }
    const char*expected=string(field(req,"bridge_instance_id"));
    if(!expected||strcmp(expected,instance)!=0)return error("bridge_instance_changed");
    if(!strcmp(method,"bridge.events.pull"))return cwr_events_pull(field(req,"payload"),instance,process_identity);
    if(!strcmp(method,"bridge.events.ack"))return cwr_events_ack(field(req,"payload"));
    if(!strcmp(method,"bridge.bindings.replace"))return cwr_bindings_replace(field(req,"payload"));
    int execute=!strcmp(method,"host.model.execute_stream"),read_stream=!strcmp(method,"host.model.stream_read"),close_stream=!strcmp(method,"host.model.stream_close");
    int model=execute||read_stream||close_stream;
    if(!model&&strcmp(method,"host.auth.list")&&strcmp(method,"host.auth.get")&&strcmp(method,"host.auth.get_runtime"))return error("method_not_allowed");
    J*payload=field(req,"payload");
    if(!payload||json_object_get_type(payload)!=4)return error("invalid_payload");
    if(execute&&!model_payload(payload))return error("invalid_payload");
    if((read_stream||close_stream)&&!bounded_text(field(payload,"stream_id"),256))return error("invalid_payload");
    if(!model&&strcmp(method,"host.auth.list")) {
        const char*index=string(field(payload,"auth_index"));
        if(!index||!index[0]||strlen(index)>256)return error("invalid_payload");
    }
    int budget=json_object_get_int(field(req,"remaining_timeout_ms"));
    if(budget<1||budget>300000)return error("invalid_deadline");
    pthread_mutex_lock(&queue_mu);
    int stopped=stopping;
    int busy=host_active>=HOST_CALLS;
    int slot=-1;
    if(!stopped&&!busy&&model) {
        for(int index=0;index<HOST_CALLS;index++) {
            if(execute?!streams[index].used:(streams[index].used&&!strcmp(streams[index].id,string(field(payload,"stream_id"))))) {
                slot=index;
                break;
            }
        }
        if(slot<0||(!execute&&(streams[slot].closing||(read_stream&&streams[slot].reading)))) {
            pthread_mutex_unlock(&queue_mu);
            return error(execute?"bridge_busy":"model_stream_unavailable");
        }
        if(execute)streams[slot].used=1;
        if(read_stream)streams[slot].reading=1;
        if(close_stream)streams[slot].closing=1;
    }
    if(!stopped&&!busy)host_active++;
    pthread_mutex_unlock(&queue_mu);
    if(stopped)return error("bridge_stopping");
    if(busy)return error("bridge_busy");
    J*response=host_callback(method,payload);
    pthread_mutex_lock(&queue_mu);
    host_active--;
    if(model) {
        struct timespec now;
        clock_gettime(CLOCK_MONOTONIC,&now);
        streams[slot].touched=now.tv_sec;
        if(execute) {
            J*id=field(field(response,"result"),"stream_id");
            if(bounded_text(id,256))strcpy(streams[slot].id,string(id));
            else {
                memset(&streams[slot],0,sizeof(streams[slot]));
                json_object_put(response);
                response=error("host_callback_failed");
            }
        }
        if(read_stream)streams[slot].reading=0;
        if(close_stream) {
            if(json_object_get_int(field(response,"ok")))streams[slot].closed=1;
            else {
                streams[slot].closing=0;
                streams[slot].retry_at=now.tv_sec+1;
            }
        }
        if(streams[slot].closed&&!streams[slot].reading)memset(&streams[slot],0,sizeof(streams[slot]));
    }
    pthread_cond_broadcast(&streams_changed);
    pthread_mutex_unlock(&queue_mu);
    if(read_stream&&field(field(response,"result"),"error")&&!equals(field(field(response,"result"),"error"),"")) {
        json_object_put(response);
        return error("host_callback_failed");
    }
    return response;
}
static void handle_fd(int fd) {
    uint32_t n;
    if(!transfer(fd,&n,4,0))return;
    size_t size=ntohl(n);
    if(!size||size>FRAME_LIMIT) {
        respond(fd,error("invalid_frame"));
        return;
    }
    char*raw=malloc(size);
    if(!raw)return;
    if(!transfer(fd,raw,size,0)) {
        free(raw);
        return;
    }
    J*req=parse(raw,size);
    memset(raw,0,size);
    free(raw);
    if(!req) {
        respond(fd,error("invalid_frame"));
        return;
    }
    J*reply=rpc(req);
    json_object_put(req);
    respond(fd,reply);
}
static void *worker(void*arg) {
    (void)arg;
    for(;;) {
        pthread_mutex_lock(&queue_mu);
        while(!stopping&&!qsize)pthread_cond_wait(&available,&queue_mu);
        if(stopping) {
            live--;
            pthread_cond_broadcast(&drained);
            pthread_mutex_unlock(&queue_mu);
            return NULL;
        }
        int fd=queued[qhead];
        qhead=(qhead+1)%QUEUE;
        qsize--;
        pthread_mutex_unlock(&queue_mu);
        handle_fd(fd);
        close(fd);
    }
}
static void *accept_loop(void*arg) {
    (void)arg;
    for(;;) {
        int fd=accept4(listener,NULL,NULL,SOCK_CLOEXEC);
        if(fd<0) {
            if(errno==EINTR)continue;
            return NULL;
        }
        struct timeval timeout= {
            3,0
        };
        setsockopt(fd,SOL_SOCKET,SO_RCVTIMEO,&timeout,sizeof(timeout));
        setsockopt(fd,SOL_SOCKET,SO_SNDTIMEO,&timeout,sizeof(timeout));
        if(!peer(fd)) {
            close(fd);
            continue;
        }
        pthread_mutex_lock(&queue_mu);
        if(stopping||qsize==QUEUE) {
            close(fd);
        }
        else {
            queued[(qhead+qsize)%QUEUE]=fd;
            qsize++;
            pthread_cond_signal(&available);
        }
        pthread_mutex_unlock(&queue_mu);
    }
}
static int stop_runtime(int bounded) {
    if(!running)return 1;
    pthread_mutex_lock(&queue_mu);
    stopping=1;
    while(qsize) {
        close(queued[qhead]);
        qhead=(qhead+1)%QUEUE;
        qsize--;
    }
    pthread_cond_broadcast(&available);
    pthread_cond_broadcast(&streams_changed);
    pthread_mutex_unlock(&queue_mu);
    shutdown(listener,SHUT_RDWR);
    struct timespec deadline;
    clock_gettime(CLOCK_REALTIME,&deadline);
    deadline.tv_sec+=5;
    pthread_mutex_lock(&queue_mu);
    while(live) {
        int result=bounded?pthread_cond_timedwait(&drained,&queue_mu,&deadline):pthread_cond_wait(&drained,&queue_mu);
        if(result==ETIMEDOUT) {
            pthread_mutex_unlock(&queue_mu);
            return 0;
        }
    }
    pthread_mutex_unlock(&queue_mu);
    pthread_join(accept_thread,NULL);
    for(int i=0;i<created;i++)pthread_join(workers[i],NULL);
    created=0;
    close(listener);
    listener=-1;
    unlink(bridge_path);
    close(owner_lock);
    owner_lock=-1;
    running=0;
    cwr_events_clear();
    return 1;
}
static int start_runtime(void) {
    if(running) {
        pthread_mutex_lock(&queue_mu);
        int active=!stopping;
        pthread_mutex_unlock(&queue_mu);
        return active;
    }
    const char*dir=getenv("CWR_V2_RUNTIME_DIR");
    struct stat st;
    if(!dir||dir[0]!='/'||lstat(dir,&st)||!S_ISDIR(st.st_mode)||(st.st_mode&0777)!=0700||st.st_uid!=geteuid())return 0;
    if(snprintf(bridge_path,sizeof(bridge_path),"%s/bridge.sock",dir)>=(int)sizeof(bridge_path)||snprintf(daemon_path,sizeof(daemon_path),"%s/daemon.sock",dir)>=(int)sizeof(daemon_path))return 0;
    char lockpath[PATH_MAX];
    snprintf(lockpath,sizeof(lockpath),"%s/.bridge.lock",dir);
    owner_lock=open(lockpath,O_CREAT|O_RDWR|O_NOFOLLOW|O_CLOEXEC,0600);
    if(owner_lock<0)return 0;
    if(flock(owner_lock,LOCK_EX|LOCK_NB)) {
        close(owner_lock);
        owner_lock=-1;
        return 0;
    }
    if(lstat(bridge_path,&st)==0) {
        if(!S_ISSOCK(st.st_mode)||st.st_uid!=geteuid())goto fail;
        int probe=socket(AF_UNIX,SOCK_STREAM|SOCK_CLOEXEC,0);
        struct sockaddr_un a= {
            .sun_family=AF_UNIX
        };
        strcpy(a.sun_path,bridge_path);
        int alive=connect(probe,(struct sockaddr*)&a,sizeof(a))==0;
        close(probe);
        if(alive||unlink(bridge_path))goto fail;
    }
    else if(errno!=ENOENT)goto fail;
    listener=socket(AF_UNIX,SOCK_STREAM|SOCK_CLOEXEC,0);
    if(listener<0)goto fail;
    struct sockaddr_un address= {
        .sun_family=AF_UNIX
    };
    strcpy(address.sun_path,bridge_path);
    if(bind(listener,(struct sockaddr*)&address,sizeof(address))||chmod(bridge_path,0600)||listen(listener,QUEUE))goto fail;
    unsigned char bytes[32];
    if(getrandom(bytes,sizeof(bytes),0)!=(ssize_t)sizeof(bytes))goto fail;
    for(int i=0;i<32;i++)snprintf(instance+i*2,3,"%02x",bytes[i]);
    cwr_events_reset();
    stopping=0;
    qsize=0;
    qhead=0;
    live=0;
    created=0;
    for(int i=0;i<WORKERS;i++) {
        pthread_mutex_lock(&queue_mu);
        live++;
        pthread_mutex_unlock(&queue_mu);
        if(pthread_create(&workers[i],NULL,i==WORKERS-1?stream_reaper:worker,NULL)) {
            pthread_mutex_lock(&queue_mu);
            live--;
            stopping=1;
            pthread_cond_broadcast(&available);
            pthread_mutex_unlock(&queue_mu);
            for(int j=0;j<created;j++)pthread_join(workers[j],NULL);
            goto fail;
        }
        created++;
    }
    if(pthread_create(&accept_thread,NULL,accept_loop,NULL)) {
        pthread_mutex_lock(&queue_mu);
        stopping=1;
        pthread_cond_broadcast(&available);
        pthread_cond_broadcast(&streams_changed);
        pthread_mutex_unlock(&queue_mu);
        for(int i=0;i<created;i++)pthread_join(workers[i],NULL);
        goto fail;
    }
    running=1;
    return 1;
    fail: if(listener>=0) {
        close(listener);
        listener=-1;
        unlink(bridge_path);
    }
    close(owner_lock);
    owner_lock=-1;
    return 0;
}
static const char b64chars[]="ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/";
static char *base64(const unsigned char*in,size_t n) {
    char*out=malloc(((n+2)/3)*4+1);
    if(!out)return NULL;
    size_t j=0;
    for(size_t i=0;i<n;i+=3) {
        uint32_t v=(uint32_t)in[i]<<16;
        if(i+1<n)v|=(uint32_t)in[i+1]<<8;
        if(i+2<n)v|=in[i+2];
        out[j++]=b64chars[(v>>18)&63];
        out[j++]=b64chars[(v>>12)&63];
        out[j++]=i+1<n?b64chars[(v>>6)&63]:'=';
        out[j++]=i+2<n?b64chars[v&63]:'=';
    }
    out[j]=0;
    return out;
}
static unsigned char *unbase64(const char*in,size_t *size,size_t limit) {
    size_t n=strlen(in);
    if(n%4||n>((limit+2)/3)*4)return NULL;
    unsigned char*out=malloc(n/4*3+1);
    if(!out)return NULL;
    size_t j=0;
    for(size_t i=0;i<n;i+=4) {
        uint32_t v=0;
        int pads=0;
        for(int k=0;k<4;k++) {
            const char*p=strchr(b64chars,in[i+k]);
            if(in[i+k]=='='&&k>=2&&i+4==n) {
                pads++;
                v<<=6;
            }
            else if(!p||in[i+k]==0||pads) {
                free(out);
                return NULL;
            }
            else v=(v<<6)|(uint32_t)(p-b64chars);
        }
        out[j++]=(v>>16)&255;
        if(pads<2)out[j++]=(v>>8)&255;
        if(pads<1)out[j++]=v&255;
    }
    if(j>limit) {
        free(out);
        return NULL;
    }
    *size=j;
    out[j]=0;
    return out;
}
typedef struct {
    char*data;
    size_t size;
}
Body;
static size_t collect(char*data,size_t a,size_t b,void*ctx) {
    Body*body=ctx;
    size_t n=a*b;
    if(a&&n/a!=b)return 0;
    if(n>HTTP_BODY_LIMIT-body->size)return 0;
    char*next=realloc(body->data,body->size+n+1);
    if(!next)return 0;
    body->data=next;
    memcpy(next+body->size,data,n);
    body->size+=n;
    next[body->size]=0;
    return n;
}
static J *management_response(int status,const char*type,const void*data,size_t size) {
    char*encoded=base64(data,size);
    if(!encoded)return error("allocation_failed");
    J*r=json_object_new_object(),*headers=json_object_new_object(),*value=json_object_new_array();
    json_object_array_add(value,json_object_new_string(type));
    json_object_object_add(headers,"Content-Type",value);
    value=json_object_new_array();
    json_object_array_add(value,json_object_new_string("no-store"));
    json_object_object_add(headers,"Cache-Control",value);
    const char*names[]= {
        "Content-Security-Policy","X-Content-Type-Options","Referrer-Policy"
    };
    const char*values[]= {
        "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; frame-ancestors 'self'; base-uri 'none'; form-action 'none'","nosniff","no-referrer"
    };
    for(int i=0;i<3;i++) {
        value=json_object_new_array();
        json_object_array_add(value,json_object_new_string(values[i]));
        json_object_object_add(headers,names[i],value);
    }
    json_object_object_add(r,"StatusCode",json_object_new_int(status));
    json_object_object_add(r,"Headers",headers);
    json_object_object_add(r,"Body",json_object_new_string(encoded));
    free(encoded);
    return ok(r);
}
static J *http_error(int status,const char*code) {
    char body[128];
    int n=snprintf(body,sizeof(body),"{\"error\":\"%s\"}",code);
    return management_response(status,"application/json",body,(size_t)n);
}
static J *proxy(J*req) {
    if(!running||stopping)return http_error(503,"bridge_stopping");
    struct stat endpoint_info;
    if(lstat(daemon_path,&endpoint_info)||!S_ISSOCK(endpoint_info.st_mode)||endpoint_info.st_uid!=geteuid())return http_error(503,"backend_unavailable");
    const char*method=string(field(req,"Method")),*path=string(field(req,"Path"));
    if(!method||!path)return error("invalid_management_request");
    const char*tail=NULL,*type="application/json; charset=utf-8";
    char endpoint[128];
    if(!strncmp(path,MGMT,strlen(MGMT))) {
        tail=path+strlen(MGMT);
        int readable=!strcmp(tail,"overview")||!strcmp(tail,"accounts")||!strcmp(tail,"jobs")||!strcmp(tail,"settings")||!strcmp(tail,"model-settings")||!strcmp(tail,"prompt-generations");
        int writable=!strcmp(tail,"jobs")||!strcmp(tail,"sync")||!strcmp(tail,"settings")||!strcmp(tail,"model-settings")||!strcmp(tail,"prompt-generations");
        if(!((!strcmp(method,"GET")&&readable)||(!strcmp(method,"POST")&&writable)))return http_error(404,"not_found");
        snprintf(endpoint,sizeof(endpoint),"/api/v2/%s",tail);
    }
    else if(!strncmp(path,RESOURCE,strlen(RESOURCE))&&strcmp(method,"GET")==0) {
        tail=path+strlen(RESOURCE);
        if(!strcmp(tail,"healthz"))snprintf(endpoint,sizeof(endpoint),"/healthz");
        else {
            if(strcmp(tail,"status")&&strcmp(tail,"panel.js")&&strcmp(tail,"panel.css"))return http_error(404,"not_found");
            snprintf(endpoint,sizeof(endpoint),"/ui/%s",tail);
            type=strcmp(tail,"status")==0?"text/html; charset=utf-8":strcmp(tail,"panel.js")==0?"text/javascript; charset=utf-8":"text/css; charset=utf-8";
        }
    }
    else return http_error(404,"not_found");
    CURL*curl=curl_easy_init();
    if(!curl)return http_error(503,"backend_unavailable");
    char target[8192];
    snprintf(target,sizeof(target),"http://localhost%s",endpoint);
    J*query=field(req,"Query");
    const char*allowed[]= {
        "job_id","after","limit","revision","search","status"
    };
    for(int i=0;i<6;i++) {
        J*v=field(query,allowed[i]);
        if(!v)continue;
        if(json_object_get_type(v)!=5||json_object_array_length(v)!=1)goto invalid;
        const char*text=string(json_object_array_get_idx(v,0));
        if(!text||strlen(text)>256)goto invalid;
        char*escaped=curl_easy_escape(curl,text,0);
        if(!escaped)goto invalid;
        size_t used=strlen(target);
        snprintf(target+used,sizeof(target)-used,"%c%s=%s",strchr(target,'?')?'&':'?',allowed[i],escaped);
        curl_free(escaped);
    }
    size_t body_size=0;
    unsigned char*request_body=NULL;
    const char*body=string(field(req,"Body"));
    if(body) {
        request_body=unbase64(body,&body_size,!strcmp(endpoint,"/api/v2/model-settings")?(128u<<10):6144);
        if(!request_body)goto invalid;
    }
    struct curl_slist*headers=NULL;
    headers=curl_slist_append(headers,"Content-Type: application/json");
    headers=curl_slist_append(headers,"Expect:");
    J*incoming=field(req,"Headers"),*key=field(incoming,"Idempotency-Key");
    if(key) {
        if(json_object_get_type(key)!=5||json_object_array_length(key)!=1)goto invalid_headers;
        const char*k=string(json_object_array_get_idx(key,0));
        if(!k||!k[0]||strlen(k)>128)goto invalid_headers;
        for(const unsigned char*p=(const unsigned char*)k;*p;p++)if(*p<33||*p>126)goto invalid_headers;
        char line[160];
        snprintf(line,sizeof(line),"Idempotency-Key: %s",k);
        headers=curl_slist_append(headers,line);
    }
    Body response= {
        0
    };
    curl_easy_setopt(curl,CURLOPT_UNIX_SOCKET_PATH,daemon_path);
    curl_easy_setopt(curl,CURLOPT_URL,target);
    curl_easy_setopt(curl,CURLOPT_PROXY,"");
    curl_easy_setopt(curl,CURLOPT_NOSIGNAL,1L);
    curl_easy_setopt(curl,CURLOPT_TIMEOUT_MS,5000L);
    curl_easy_setopt(curl,CURLOPT_CONNECTTIMEOUT_MS,500L);
    curl_easy_setopt(curl,CURLOPT_HTTPHEADER,headers);
    curl_easy_setopt(curl,CURLOPT_WRITEFUNCTION,collect);
    curl_easy_setopt(curl,CURLOPT_WRITEDATA,&response);
    if(strcmp(method,"POST")==0) {
        curl_easy_setopt(curl,CURLOPT_POST,1L);
        curl_easy_setopt(curl,CURLOPT_POSTFIELDS,request_body?(char*)request_body:"");
        curl_easy_setopt(curl,CURLOPT_POSTFIELDSIZE,(long)body_size);
    }
    CURLcode result=curl_easy_perform(curl);
    long status=0;
    char*content_type=NULL;
    curl_easy_getinfo(curl,CURLINFO_RESPONSE_CODE,&status);
    curl_easy_getinfo(curl,CURLINFO_CONTENT_TYPE,&content_type);
    char*copied_type=content_type&&strlen(content_type)<256?strdup(content_type):NULL;
    curl_slist_free_all(headers);
    curl_easy_cleanup(curl);
    free(request_body);
    if(result!=CURLE_OK||status<100||status>599) {
        free(copied_type);
        free(response.data);
        return http_error(result==CURLE_OPERATION_TIMEDOUT?504:503,"backend_unavailable");
    }
    J*reply=management_response((int)status,copied_type?copied_type:type,response.data,response.size);
    free(copied_type);
    free(response.data);
    return reply;
    invalid_headers:curl_slist_free_all(headers);
    free(request_body);
    invalid:curl_easy_cleanup(curl);
    return http_error(400,"invalid_management_request");
}
static J *registration(void) {
    J*r=json_object_new_object(),*metadata=json_object_new_object(),*cap=json_object_new_object();
    json_object_object_add(r,"schema_version",json_object_new_int(6));
    json_object_object_add(metadata,"Name",json_object_new_string("codex-window-refresh"));
    json_object_object_add(metadata,"Version",json_object_new_string(CWR_VERSION));
    json_object_object_add(metadata,"Author",json_object_new_string("codex-window-refresh contributors"));
    json_object_object_add(metadata,"GitHubRepository",json_object_new_string("https://github.com/JayConstruct/codex-window-refresh"));
    json_object_object_add(metadata,"ConfigFields",json_object_new_array());
    json_object_object_add(r,"metadata",metadata);
    json_object_object_add(cap,"management_api",json_object_new_boolean(1));
    json_object_object_add(cap,"request_interceptor",json_object_new_boolean(1));
    json_object_object_add(cap,"request_lifecycle_plugin",json_object_new_boolean(1));
    json_object_object_add(cap,"response_interceptor",json_object_new_boolean(1));
    json_object_object_add(cap,"response_stream_interceptor",json_object_new_boolean(1));
    json_object_object_add(cap,"websocket_response_observer",json_object_new_boolean(1));
    json_object_object_add(r,"capabilities",cap);
    return ok(r);
}
static J *routes(void) {
    J*r=json_object_new_object(),*list=json_object_new_array(),*resources=json_object_new_array();
    const char*paths[]= {
        "overview","accounts","jobs","jobs","sync","settings","settings","model-settings","model-settings","prompt-generations","prompt-generations"
    };
    for(int i=0;i<11;i++) {
        J*item=json_object_new_object();
        char path[256];
        snprintf(path,sizeof(path),"%s%s",MGMT,paths[i]);
        json_object_object_add(item,"Method",json_object_new_string(i==3||i==4||i==6||i==8||i==10?"POST":"GET"));
        json_object_object_add(item,"Path",json_object_new_string(path));
        json_object_array_add(list,item);
    }
    const char*assets[]= {
        "/status","/panel.js","/panel.css","/healthz"
    };
    for(int i=0;i<4;i++) {
        J*item=json_object_new_object();
        json_object_object_add(item,"Path",json_object_new_string(assets[i]));
        if(!i)json_object_object_add(item,"Menu",json_object_new_string("Codex Window Refresh v2"));
        json_object_array_add(resources,item);
    }
    json_object_object_add(r,"Routes",list);
    json_object_object_add(r,"Resources",resources);
    return ok(r);
}
static int plugin_call(char*method,uint8_t*payload,size_t size,Buffer*out) {
    if(!out)return 1;
    out->ptr=NULL;
    out->len=0;
    if(!method||!method[0]||strnlen(method,257)>256||(size&&!payload))return 1;
    int hook=!strcmp(method,"request.intercept_before")||!strcmp(method,"request.intercept_after")||!strcmp(method,"request.complete")||!strcmp(method,"response.intercept_after")||!strcmp(method,"response.intercept_stream_chunk")||!strcmp(method,"websocket.response_event");
    if(hook) {
        pthread_rwlock_rdlock(&gate);
        if(running&&!stopping)cwr_observe_hook(method,size<=(64u<<20)?payload:NULL,size<=(64u<<20)?size:0);
        // SDK RequestInterceptResponse only replaces non-empty bodies and
        // explicitly mentioned headers. Empty modifications preserve both.
        J*reply=ok(json_object_new_object());
        int status=write_buffer(out,reply);
        json_object_put(reply);
        pthread_rwlock_unlock(&gate);
        return status;
    }
    if(size>FRAME_LIMIT)return 1;
    int lifecycle=!strcmp(method,"plugin.register")||!strcmp(method,"plugin.reconfigure")||!strcmp(method,"plugin.quiesce")||!strcmp(method,"plugin.shutdown");
    if(lifecycle)pthread_rwlock_wrlock(&gate);
    else pthread_rwlock_rdlock(&gate);
    J*req=parse(payload,size),*reply=NULL;
    if(!req)reply=error("invalid_payload");
    else if(!strcmp(method,"plugin.register")||!strcmp(method,"plugin.reconfigure")) {
        if(json_object_get_int(field(req,"schema_version"))<6)reply=error("schema_incompatible");
        else if(running&&stopping&&!stop_runtime(1))reply=error("bridge_still_draining");
        else if(!start_runtime())reply=error("bridge_initialization_failed");
        else reply=registration();
    }
    else if(!strcmp(method,"plugin.quiesce")||!strcmp(method,"plugin.shutdown")) {
        reply=stop_runtime(1)?ok(json_object_new_object()):error("bridge_still_draining");
    }
    else if(!strcmp(method,"management.register"))reply=routes();
    else if(!strcmp(method,"management.handle"))reply=proxy(req);
    else reply=error("method_not_allowed");
    int status=write_buffer(out,reply);
    json_object_put(reply);
    json_object_put(req);
    pthread_rwlock_unlock(&gate);
    return status;
}
static void plugin_shutdown(void) {
    pthread_rwlock_wrlock(&gate);
    stop_runtime(0);
    initialized=0;
    memset(&host,0,sizeof(host));
    curl_global_cleanup();
    pthread_rwlock_unlock(&gate);
}
__attribute__((visibility("default"))) int cliproxy_plugin_init(const HostAPI*input,PluginAPI*api) {
    if(!input||!api||input->abi_version!=1||!input->host_ctx||!input->call||!input->free_buffer)return 1;
    pthread_rwlock_wrlock(&gate);
    if(initialized) {
        pthread_rwlock_unlock(&gate);
        return 1;
    }
    if(!identify_process()) {
        pthread_rwlock_unlock(&gate);
        return 1;
    }
    if(curl_global_init(CURL_GLOBAL_DEFAULT)!=CURLE_OK) {
        pthread_rwlock_unlock(&gate);
        return 1;
    }
    host=*input;
    initialized=1;
    *api=(PluginAPI) {
        1,plugin_call,release,plugin_shutdown
    };
    pthread_rwlock_unlock(&gate);
    return 0;
}
