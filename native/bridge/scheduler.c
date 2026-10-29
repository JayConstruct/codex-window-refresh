/* Bounded, memory-only scheduler. No host callback, socket or filesystem I/O
 * occurs on the request path. The daemon renews this snapshot every 2s. */
#define _POSIX_C_SOURCE 200809L
#include "scheduler.h"
#include <pthread.h>
#include <string.h>
#include <time.h>
#define SLOTS 4096
static pthread_mutex_t lock=PTHREAD_MUTEX_INITIALIZER;
static J *snapshot;
static time_t deadline;
static unsigned cursor;
typedef struct {char scope[257],session[257],project[257],account[257];time_t expires;} Affinity;
typedef struct {char request[257],account[257];time_t expires;} Active;
static Affinity sessions[SLOTS];
static Active active[SLOTS];
static struct {char account[257];time_t expires;} pending[SLOTS];
static unsigned session_cursor,active_cursor,pending_cursor;
static J *get(J *j,const char *name){J*v=NULL;if(j&&json_object_get_type(j)==4)json_object_object_get_ex(j,name,&v);return v;}
static size_t length(J*j){return j&&json_object_get_type(j)==5?json_object_array_length(j):0;}
static const char *text(J*j){const char*s=json_object_get_type(j)==6?json_object_get_string(j):NULL;return s&&s[0]&&strlen(s)<=256&&strlen(s)==(size_t)json_object_get_string_len(j)?s:NULL;}
static time_t monotonic(void){struct timespec t;clock_gettime(CLOCK_MONOTONIC,&t);return t.tv_sec;}
static J *reply(J*r){J*j=json_object_new_object();json_object_object_add(j,"ok",json_object_new_boolean(1));json_object_object_add(j,"result",r);return j;}
static const char *header(J*headers,const char*a,const char*b,const char*c){J*v=get(headers,a);if(!v)v=get(headers,b);if(!v)v=get(headers,c);if(json_object_get_type(v)!=5||length(v)!=1)return NULL;return text(json_object_array_get_idx(v,0));}
void cwr_scheduler_clear(void){pthread_mutex_lock(&lock);json_object_put(snapshot);snapshot=NULL;deadline=0;memset(sessions,0,sizeof(sessions));memset(active,0,sizeof(active));memset(pending,0,sizeof(pending));pthread_mutex_unlock(&lock);}
J *cwr_scheduler_replace(J*input){
    J*config=get(input,"config"),*accounts=get(input,"accounts"),*projects=get(config,"projects"),*blocked=get(input,"blocked");
    if(blocked&&json_object_get_type(blocked)!=0&&(json_object_get_type(blocked)!=5||length(blocked)>10000))return NULL;
    for(size_t i=0;i<length(blocked);i++)if(!text(json_object_array_get_idx(blocked,i))||(i&&strcmp(text(json_object_array_get_idx(blocked,i-1)),text(json_object_array_get_idx(blocked,i)))>=0))return NULL;
    int cap=json_object_get_int(get(config,"max_inflight")),ttl=json_object_get_int(get(config,"session_ttl_seconds"));
    if(json_object_get_type(config)!=4||json_object_get_type(accounts)!=5||length(accounts)>10000||json_object_get_type(projects)!=5||length(projects)>128||cap<1||cap>128||ttl<1||ttl>86400)return NULL;
    for(size_t i=0;i<length(accounts);i++){J*a=json_object_array_get_idx(accounts,i);if(!text(get(a,"id"))||json_object_get_type(get(a,"valid_until"))!=3)return NULL;if(i&&strcmp(text(get(json_object_array_get_idx(accounts,i-1),"id")),text(get(a,"id")))>=0)return NULL;}
    for(size_t i=0;i<length(projects);i++){J*p=json_object_array_get_idx(projects,i),*ids=get(p,"accounts");if(!text(get(p,"project"))||json_object_get_type(ids)!=5||length(ids)>1000)return NULL;for(size_t k=0;k<length(ids);k++)if(!text(json_object_array_get_idx(ids,k)))return NULL;}
    pthread_mutex_lock(&lock);json_object_put(snapshot);snapshot=json_object_get(input);deadline=monotonic()+10;pthread_mutex_unlock(&lock);return reply(json_object_new_object());
}
void cwr_scheduler_observe(const char*request,const char*account,int completed){
    if(!request||strlen(request)>256)return;
    pthread_mutex_lock(&lock);time_t now=monotonic();
    for(unsigned i=0;i<SLOTS;i++)if(!strcmp(active[i].request,request)){active[i].expires=0;if(completed){pthread_mutex_unlock(&lock);return;}break;}
    if(!completed&&account&&strlen(account)<=256){unsigned chosen=active_cursor++%SLOTS;for(unsigned i=0;i<SLOTS;i++)if(active[i].expires<=now){chosen=i;break;}strcpy(active[chosen].request,request);strcpy(active[chosen].account,account);active[chosen].expires=now+600;
        for(unsigned i=0;i<SLOTS;i++)if(pending[i].expires>now&&!strcmp(pending[i].account,account)){pending[i].expires=0;break;}}
    pthread_mutex_unlock(&lock);
}
static int account_index(J*accounts,const char*id){
    int low=0,high=(int)length(accounts)-1;
    while(low<=high){int mid=low+(high-low)/2;int order=strcmp(text(get(json_object_array_get_idx(accounts,(size_t)mid),"id")),id);if(!order)return mid;if(order<0)low=mid+1;else high=mid-1;}return -1;
}
static int contains_sorted(J*list,const char*id){
    int low=0,high=(int)length(list)-1;while(low<=high){int mid=low+(high-low)/2;int order=strcmp(text(json_object_array_get_idx(list,(size_t)mid)),id);if(!order)return 1;if(order<0)low=mid+1;else high=mid-1;}return 0;
}
J *cwr_scheduler_pick(J*request){
    J*result=json_object_new_object();pthread_mutex_lock(&lock);time_t now=monotonic(),wall=time(NULL);
    if(!snapshot||deadline<=now)goto done;
    J*config=get(snapshot,"config");if(!json_object_get_int(get(config,"enabled")))goto done;
    J*candidates=get(request,"Candidates"),*options=get(request,"Options");if(!candidates)candidates=get(request,"candidates");if(!options)options=get(request,"options");
    J*metadata=get(options,"Metadata"),*headers=get(options,"Headers");if(!metadata)metadata=get(options,"metadata");if(!headers)headers=get(options,"headers");
    if(json_object_get_type(candidates)!=5||length(candidates)>10000)goto done;
    const char*project=header(headers,"X-Cwr-Project","x-cwr-project","X-CWR-Project");if(!project)project="";
    const char*session=text(get(metadata,"session_id"));if(!session)session=header(headers,"Session_id","session_id","Session-Id");
    const char*scope=text(get(metadata,"caller_scope"));J*preference=NULL,*projects=get(config,"projects");
    for(size_t i=0;i<length(projects);i++){J*p=json_object_array_get_idx(projects,i);const char*label=text(get(p,"project"));if(label&&!strcmp(label,project)){preference=get(p,"accounts");break;}}
    const char*bound=NULL;for(unsigned i=0;scope&&session&&i<SLOTS;i++)if(sessions[i].expires>now&&!strcmp(sessions[i].scope,scope)&&!strcmp(sessions[i].session,session)&&!strcmp(sessions[i].project,project)){bound=sessions[i].account;break;}
    J*accounts=get(snapshot,"accounts");unsigned inflight[10000]={0};unsigned char groups[10000]={0};
    for(size_t i=0;i<length(preference);i++){int n=account_index(accounts,text(json_object_array_get_idx(preference,i)));if(n>=0)groups[n]=1;}
    for(unsigned i=0;i<SLOTS;i++){if(active[i].expires>now){int n=account_index(accounts,active[i].account);if(n>=0)inflight[n]++;}if(pending[i].expires>now){int n=account_index(accounts,pending[i].account);if(n>=0)inflight[n]++;}}
    const char*best=NULL;int best_group=-1,best_affinity=-1;int64_t best_deadline=INT64_MAX;unsigned best_load=~0u;int known=0;
    size_t total=length(candidates),start=total?cursor++%total:0;
    for(size_t k=0;k<total;k++){J*c=json_object_array_get_idx(candidates,(start+k)%total);const char*id=text(get(c,"ID"));if(!id)id=text(get(c,"id"));if(!id)continue;const char*provider=text(get(c,"Provider"));if(!provider)provider=text(get(c,"provider"));if(!provider||strcmp(provider,"codex"))continue;
        if(contains_sorted(get(snapshot,"blocked"),id)){known++;continue;}
        int index=account_index(accounts,id);J*a=index>=0?json_object_array_get_idx(accounts,(size_t)index):NULL;
        if(!a||json_object_get_int64(get(a,"valid_until"))<=wall)continue;
        known++;
        unsigned busy=inflight[index];if(busy>=(unsigned)json_object_get_int(get(config,"max_inflight")))continue;
        int group=groups[index],affinity=bound&&!strcmp(bound,id);int64_t reset=json_object_get_int64(get(a,"reset_at")),card=json_object_get_int64(get(a,"card_expiry")),urgency=INT64_MAX;
        if(json_object_get_int(get(config,"prefer_reset"))&&reset>wall)urgency=reset;
        if(card>wall&&card<urgency)urgency=card;
        if(!best||group>best_group||(group==best_group&&(affinity>best_affinity||(affinity==best_affinity&&(urgency<best_deadline||(urgency==best_deadline&&busy<best_load)))))){best=id;best_group=group;best_affinity=affinity;best_deadline=urgency;best_load=busy;}
    }
    if(best){
        json_object_object_add(result,"Handled",json_object_new_boolean(1));json_object_object_add(result,"AuthID",json_object_new_string(best));
        unsigned slot=pending_cursor++%SLOTS;for(unsigned i=0;i<SLOTS;i++)if(pending[i].expires<=now){slot=i;break;}strcpy(pending[slot].account,best);pending[slot].expires=now+15;
        if(scope&&session){unsigned at=session_cursor++%SLOTS;for(unsigned i=0;i<SLOTS;i++)if(sessions[i].expires<=now||(!strcmp(sessions[i].scope,scope)&&!strcmp(sessions[i].session,session)&&!strcmp(sessions[i].project,project))){at=i;break;}strcpy(sessions[at].scope,scope);strcpy(sessions[at].session,session);strcpy(sessions[at].project,project);strcpy(sessions[at].account,best);sessions[at].expires=now+json_object_get_int(get(config,"session_ttl_seconds"));}
    }else if(known){json_object_object_add(result,"Handled",json_object_new_boolean(1));json_object_object_add(result,"Reject",json_object_new_boolean(1));json_object_object_add(result,"RejectCode",json_object_new_string("scheduler_busy"));}
done:
    pthread_mutex_unlock(&lock);return reply(result);
}
