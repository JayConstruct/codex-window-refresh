/* Bounded, memory-only Codex routing and affinity. No request body, disk,
 * socket or host callback is used here. CPA supplies canonical session IDs. */
#define _POSIX_C_SOURCE 200809L
#include "scheduler.h"
#include "events.h"
#include <pthread.h>
#include <stdint.h>
#include <stdlib.h>
#include <stdio.h>
#include <string.h>
#include <strings.h>
#include <time.h>

#define SLOTS 4096
#define LABEL 257
#define MAX_ACCOUNTS 10000
typedef struct {
    char scope[LABEL],project[LABEL],provider[LABEL],model[LABEL],session[LABEL];
} SessionKey;
typedef struct {
    SessionKey key;
    char account[LABEL],binding[LABEL],failed_account[LABEL];
    time_t expires,failed_until;
    uint64_t generation;
    int detached;
} Affinity;
typedef struct {
    char id[LABEL],account[LABEL],selection[LABEL],reason[64];
    time_t expires;
    int session_slot;
    uint64_t generation;
} Execution;
static pthread_mutex_t lock=PTHREAD_MUTEX_INITIALIZER;
static J *snapshot;
static time_t deadline;
static unsigned cursor;
static uint64_t generation;
static Affinity sessions[SLOTS];
static Execution active[SLOTS],pending[SLOTS],history[SLOTS];
static unsigned history_cursor;
static struct {char project[69];time_t seen;} observed_projects[1024];

static J *get(J *object,const char *name) {
    J *value=NULL;
    if(object&&json_object_get_type(object)==4)json_object_object_get_ex(object,name,&value);
    return value;
}
static J *either(J *object,const char *name,const char *alternate) {
    J *value=get(object,name);return value?value:get(object,alternate);
}
static size_t length(J *value) {return value&&json_object_get_type(value)==5?json_object_array_length(value):0;}
static const char *text(J *value) {
    const char *s=value&&json_object_get_type(value)==6?json_object_get_string(value):NULL;
    if(!s||!s[0]||strlen(s)>256||strlen(s)!=(size_t)json_object_get_string_len(value))return NULL;
    for(const unsigned char *c=(const unsigned char*)s;*c;c++)if(*c<32||*c==127)return NULL;
    return s;
}
static time_t monotonic(void) {struct timespec t;clock_gettime(CLOCK_MONOTONIC,&t);return t.tv_sec;}
static J *reply(J *result) {
    J *response=json_object_new_object();
    json_object_object_add(response,"ok",json_object_new_boolean(1));
    json_object_object_add(response,"result",result);return response;
}
static void reject(J *result,const char *code) {
    json_object_object_add(result,"Handled",json_object_new_boolean(1));
    json_object_object_add(result,"Reject",json_object_new_boolean(1));
    json_object_object_add(result,"RejectCode",json_object_new_string(code));
}
/* Header names are case insensitive; duplicates are never identities. */
static const char *header(J *headers,const char *name) {
    if(!headers||json_object_get_type(headers)!=4)return NULL;
    const char *value=NULL;
    struct json_object_iterator it=json_object_iter_begin(headers),end=json_object_iter_end(headers);
    while(!json_object_iter_equal(&it,&end)) {
        if(!strcasecmp(json_object_iter_peek_name(&it),name)) {
            J *values=json_object_iter_peek_value(&it);
            if(value||length(values)!=1)return NULL;
            value=text(json_object_array_get_idx(values,0));if(!value)return NULL;
        }
        json_object_iter_next(&it);
    }
    return value;
}
static int same_namespace(const SessionKey *a,const SessionKey *b) {
    return !strcmp(a->scope,b->scope)&&!strcmp(a->project,b->project)&&!strcmp(a->provider,b->provider);
}
static int same_key(const SessionKey *a,const SessionKey *b) {
    return same_namespace(a,b)&&!strcmp(a->model,b->model)&&!strcmp(a->session,b->session);
}
static int find_session(const SessionKey *key,time_t now) {
    if(!key->scope[0]||!key->session[0]||!key->model[0])return -1;
    for(unsigned i=0;i<SLOTS;i++)if(sessions[i].expires>now&&same_key(&sessions[i].key,key))return (int)i;
    return -1;
}
static int object_index(J *list,const char *id) {
    int low=0,high=(int)length(list)-1;
    while(low<=high) {
        int mid=low+(high-low)/2;
        int order=strcmp(text(get(json_object_array_get_idx(list,(size_t)mid),"id")),id);
        if(!order)return mid;
        if(order<0)low=mid+1;else high=mid-1;
    }
    return -1;
}
static int contains_sorted(J *list,const char *id) {
    int low=0,high=(int)length(list)-1;
    while(low<=high) {
        int mid=low+(high-low)/2;
        int order=strcmp(text(json_object_array_get_idx(list,(size_t)mid)),id);
        if(!order)return 1;
        if(order<0)low=mid+1;else high=mid-1;
    }
    return 0;
}
static J *identity(const char *id) {
    J *list=get(snapshot,"identities");int index=object_index(list,id);
    return index<0?NULL:json_object_array_get_idx(list,(size_t)index);
}
static int invalid_binding(Affinity *binding) {
    if(deadline<=monotonic())return 0;
    J *current=identity(binding->account);
    const char *digest=text(get(current,"binding")),*status=text(get(current,"status"));
    return !digest||strcmp(digest,binding->binding)||!status||!strcmp(status,"disabled")||!strcmp(status,"removed")||!strcmp(status,"binding_unavailable")||(deadline>monotonic()&&contains_sorted(get(snapshot,"exhausted"),binding->account));
}
static void detach(Affinity *binding,time_t now,int failed) {
    if(failed&&binding->account[0]) {
        strcpy(binding->failed_account,binding->account);binding->failed_until=now+30;
    }
    binding->account[0]=0;binding->binding[0]=0;binding->detached=1;
    binding->generation=++generation;
}
static int bind_session(const SessionKey *key,const char *account,time_t now,int ttl,int detached) {
    if(!key->scope[0]||!key->session[0]||!key->model[0])return -1;
    const char *digest=text(get(identity(account),"binding"));if(!digest)return -1;
    int found=find_session(key,now);
    unsigned slot=found>=0?(unsigned)found:0;
    if(found<0)for(unsigned i=0;i<SLOTS;i++) {
        if(sessions[i].expires<=now){slot=i;break;}
        if(sessions[i].expires<sessions[slot].expires)slot=i;
    }
    Affinity *binding=&sessions[slot];if(found<0)memset(binding,0,sizeof(*binding));
    if(found<0||strcmp(binding->account,account)||strcmp(binding->binding,digest))binding->generation=++generation;
    binding->key=*key;strcpy(binding->account,account);strcpy(binding->binding,digest);
    binding->expires=now+ttl;binding->detached|=detached;
    return (int)slot;
}
static int boolean_default(J *config,const char *name,int fallback) {
    J *value=get(config,name);return value?json_object_get_int(value):fallback;
}
static int sorted_objects(J *list,int require_binding) {
    if(!list||json_object_get_type(list)!=5||length(list)>MAX_ACCOUNTS)return 0;
    for(size_t i=0;i<length(list);i++) {
        J *item=json_object_array_get_idx(list,i);const char *id=text(get(item,"id"));
        if(!id||(require_binding&&!text(get(item,"binding"))))return 0;
        if(i&&strcmp(text(get(json_object_array_get_idx(list,i-1),"id")),id)>=0)return 0;
    }
    return 1;
}
static int sorted_strings(J *list) {
    if(!list||json_object_get_type(list)==0)return 1;
    if(json_object_get_type(list)!=5||length(list)>MAX_ACCOUNTS)return 0;
    for(size_t i=0;i<length(list);i++) {
        const char *id=text(json_object_array_get_idx(list,i));
        if(!id||(i&&strcmp(text(json_object_array_get_idx(list,i-1)),id)>=0))return 0;
    }
    return 1;
}
void cwr_scheduler_clear(void) {
    pthread_mutex_lock(&lock);json_object_put(snapshot);snapshot=NULL;deadline=0;
    memset(sessions,0,sizeof(sessions));memset(active,0,sizeof(active));memset(pending,0,sizeof(pending));
    memset(history,0,sizeof(history));
    memset(observed_projects,0,sizeof(observed_projects));
    pthread_mutex_unlock(&lock);
}
J *cwr_scheduler_replace(J *input) {
    J *config=get(input,"config"),*accounts=get(input,"accounts"),*projects=get(config,"projects"),*identities=get(input,"identities");
    int cap=json_object_get_int(get(config,"max_inflight")),ttl=json_object_get_int(get(config,"session_ttl_seconds"));
    const char *rebind=text(get(config,"rebind_on"));
    if(!config||json_object_get_type(config)!=4||!sorted_objects(accounts,0)||!sorted_objects(identities,1)||!sorted_strings(get(input,"blocked"))||!sorted_strings(get(input,"exhausted"))||!projects||json_object_get_type(projects)!=5||length(projects)>128||cap<1||cap>128||ttl<1||ttl>86400||(rebind&&strcmp(rebind,"quota")&&strcmp(rebind,"any")))return NULL;
    for(size_t i=0;i<length(accounts);i++)if(json_object_get_type(get(json_object_array_get_idx(accounts,i),"valid_until"))!=3)return NULL;
    for(size_t i=0;i<length(identities);i++)if(!text(get(json_object_array_get_idx(identities,i),"status")))return NULL;
    for(size_t i=0;i<length(projects);i++) {
        J *p=json_object_array_get_idx(projects,i),*ids=get(p,"accounts");
        if(!text(get(p,"project"))||!ids||json_object_get_type(ids)!=5||length(ids)>1000)return NULL;
        for(size_t k=0;k<length(ids);k++)if(!text(json_object_array_get_idx(ids,k)))return NULL;
    }
    pthread_mutex_lock(&lock);
    if(snapshot&&json_object_get_int(get(get(snapshot,"config"),"enabled"))!=json_object_get_int(get(config,"enabled")))memset(sessions,0,sizeof(sessions));
    json_object_put(snapshot);snapshot=json_object_get(input);deadline=monotonic()+10;
    time_t now=monotonic();
    for(unsigned i=0;i<SLOTS;i++)if(sessions[i].expires>now&&sessions[i].account[0]&&invalid_binding(&sessions[i]))detach(&sessions[i],now,0);
    pthread_mutex_unlock(&lock);return reply(json_object_new_object());
}

/* Keep per-selection ownership through retries without retaining headers or
 * payloads. A delayed completion may not invalidate a newer session choice. */
static void remember(Execution *execution) {
    history[history_cursor++%SLOTS]=*execution;
}
static void failed_execution(Execution *execution,time_t now,int status) {
    if(execution->session_slot<0)return;
    Affinity *binding=&sessions[execution->session_slot];
    const char *rebind=text(get(get(snapshot,"config"),"rebind_on"));
    int hard=status==401||status==403||status==429;
    int transient=status==0||status==408||status>=500;
    if(binding->expires>now&&binding->generation==execution->generation&&!strcmp(binding->account,execution->account)&&(hard||(transient&&rebind&&!strcmp(rebind,"any"))))detach(binding,now,1);
}
/* Usage IDs differ from the handler RequestID. Only the host's selection ID
 * joins a single upstream attempt to its reservation and binding generation. */
void cwr_scheduler_usage(const char *selection,const char *account,int failed,int status) {
    if(!selection||!account)return;
    pthread_mutex_lock(&lock);time_t now=monotonic();Execution *execution=NULL;
    for(unsigned i=0;i<SLOTS;i++)if(active[i].expires>now&&!strcmp(active[i].selection,selection)&&!strcmp(active[i].account,account)){execution=&active[i];break;}
    if(execution) {
        /* Status zero may mean client cancellation. Leave that decision to
         * request.complete, whose outcome distinguishes canceled from failed. */
        if(failed&&status)failed_execution(execution,now,status);
        remember(execution);execution->expires=0;
    } else for(unsigned i=0;i<SLOTS;i++)if(history[i].expires>now&&!strcmp(history[i].selection,selection)&&!strcmp(history[i].account,account)) {
        if(failed&&status)failed_execution(&history[i],now,status);
        break;
    }
    pthread_mutex_unlock(&lock);
}
void cwr_scheduler_observe(const char *request,const char *account,const char *selection,int completed,const char *outcome,int status) {
    if(!request||strlen(request)>256)return;
    pthread_mutex_lock(&lock);time_t now=monotonic();Execution *execution=NULL;
    for(unsigned i=0;i<SLOTS;i++)if(active[i].expires>now&&!strcmp(active[i].id,request)){execution=&active[i];break;}
    if(completed) {
        if(!execution&&selection)for(unsigned i=0;i<SLOTS;i++)if(history[i].expires>now&&!strcmp(history[i].id,request)&&!strcmp(history[i].selection,selection)){execution=&history[i];break;}
        if(execution) {
            int owns=(!account||!strcmp(account,execution->account))&&(!selection||!strcmp(selection,execution->selection));
            if(owns&&outcome&&!strcmp(outcome,"failed"))failed_execution(execution,now,status);
            if(owns){remember(execution);execution->expires=0;}
        }
    } else if(account&&strlen(account)<=256) {
        if(execution&&selection&&!strcmp(execution->selection,selection)&&!strcmp(execution->account,account)){execution->expires=now+600;pthread_mutex_unlock(&lock);return;}
        if(execution){remember(execution);execution->expires=0;}
        unsigned slot=SLOTS,occupied=0;
        for(unsigned i=0;i<SLOTS;i++) {
            if(active[i].expires<=now)slot=i;else occupied++;
            if(pending[i].expires>now)occupied++;
        }
        Execution chosen={.session_slot=-1};
        int reserved=0;
        if(selection)for(unsigned i=0;i<SLOTS;i++)if(pending[i].expires>now&&!strcmp(pending[i].id,selection)&&!strcmp(pending[i].account,account)){chosen=pending[i];pending[i].expires=0;reserved=1;break;}
        if(slot<SLOTS&&(reserved||(occupied<SLOTS&&identity(account)))) {
            if(selection&&strlen(selection)<=256)strcpy(chosen.selection,selection);
            strcpy(chosen.id,request);strcpy(chosen.account,account);chosen.expires=now+600;active[slot]=chosen;
        }
    }
    pthread_mutex_unlock(&lock);
}
static void session_identity(J *metadata,J *headers,char *session,char *parent) {
    /* Canonical metadata covers explicit body identities, forks and compaction.
     * Never replace it with a weaker header or a prompt-derived guess. */
    const char *id=text(get(metadata,"canonical_session_id"));
    if(!id)id=text(get(metadata,"session_id"));
    if(!id)id=text(get(metadata,"execution_session_id"));
    if(!id)id=text(get(metadata,"derived_session_id"));
    if(!id) {
        const char *names[]={"Session-Id","Session_id","X-Session-ID","X-Thread-Id","Thread-Id"};
        for(size_t i=0;i<sizeof(names)/sizeof(*names)&&!id;i++)id=header(headers,names[i]);
    }
    if(id)strcpy(session,id);
    const char *ancestor=text(get(metadata,"parent_session_id"));
    if(!ancestor) {
        const char *names[]={"x-codex-parent-thread-id","X-Parent-Session-Id","X-Parent-Thread-Id"};
        for(size_t i=0;i<sizeof(names)/sizeof(*names)&&!ancestor;i++)ancestor=header(headers,names[i]);
    }
    if(ancestor&&strcmp(session,ancestor))strcpy(parent,ancestor);
}
static int candidate_index(J *candidates,const char *id) {
    for(size_t i=0;i<length(candidates);i++) {
        const char *candidate=text(either(json_object_array_get_idx(candidates,i),"ID","id"));
        if(candidate&&!strcmp(candidate,id))return (int)i;
    }
    return -1;
}
static unsigned inflight(const char *account,time_t now) {
    unsigned count=0;
    for(unsigned i=0;i<SLOTS;i++) {
        if(active[i].expires>now&&!strcmp(active[i].account,account))count++;
        if(pending[i].expires>now&&!strcmp(pending[i].account,account))count++;
    }
    return count;
}
static int held(const char *account) {return contains_sorted(get(snapshot,"blocked"),account);}
static int exhausted(const char *account) {return deadline>monotonic()&&contains_sorted(get(snapshot,"exhausted"),account);}
static int forbidden(const char *account) {
    if(deadline<=monotonic())return 0;
    const char *status=text(get(identity(account),"status"));
    return exhausted(account)||(status&&(!strcmp(status,"disabled")||!strcmp(status,"removed")||!strcmp(status,"binding_unavailable")));
}
/* Cross-model parent inheritance requires agreement between the parent's
 * live bindings and membership in the child's host-filtered candidate set. */
static int parent_binding(const SessionKey *child,const char *parent,time_t now) {
    SessionKey key=*child;strcpy(key.session,parent);
    int exact=find_session(&key,now);if(exact>=0&&sessions[exact].account[0])return exact;
    int found=-1;
    for(unsigned i=0;i<SLOTS;i++)if(sessions[i].expires>now&&sessions[i].account[0]&&same_namespace(child,&sessions[i].key)&&!strcmp(parent,sessions[i].key.session)) {
        if(found>=0&&strcmp(sessions[found].account,sessions[i].account))return -1;
        found=(int)i;
    }
    return found;
}
typedef struct {const char *id;int index;} CandidateIndex;
static int compare_candidate(const void *left,const void *right) {
    const CandidateIndex *a=left,*b=right;return strcmp(a->id,b->id);
}
static int indexed_candidate(CandidateIndex *map,size_t count,const char *id) {
    CandidateIndex key={.id=id};CandidateIndex *found=bsearch(&key,map,count,sizeof(*map),compare_candidate);return found?found->index:-1;
}
static int choose_new(J *candidates,J *preference,const Affinity *own,time_t now,time_t wall) {
    int best=-1,best_priority=0,best_group=-1,best_fresh=-1;
    int64_t best_deadline=INT64_MAX;unsigned best_load=~0u;
    J *accounts=get(snapshot,"accounts"),*config=get(snapshot,"config");
    unsigned groups[MAX_ACCOUNTS]={0},loads[MAX_ACCOUNTS]={0};
    CandidateIndex map[MAX_ACCOUNTS];size_t count=0;
    for(size_t i=0;i<length(candidates);i++){const char *id=text(either(json_object_array_get_idx(candidates,i),"ID","id"));if(id)map[count++]=(CandidateIndex){id,(int)i};}
    qsort(map,count,sizeof(*map),compare_candidate);
    for(size_t i=0;i<length(preference);i++) {
        int index=indexed_candidate(map,count,text(json_object_array_get_idx(preference,i)));
        if(index>=0)groups[index]=1;
    }
    for(unsigned i=0;i<SLOTS;i++) {
        if(active[i].expires>now){int n=indexed_candidate(map,count,active[i].account);if(n>=0)loads[n]++;}
        if(pending[i].expires>now){int n=indexed_candidate(map,count,pending[i].account);if(n>=0)loads[n]++;}
    }
    size_t total=length(candidates),start=total?cursor++%total:0;
    for(size_t k=0;k<total;k++) {
        size_t i=(start+k)%total;J *candidate=json_object_array_get_idx(candidates,i);
        const char *id=text(either(candidate,"ID","id"));
        if(!id||forbidden(id)||held(id)||loads[i]>=(unsigned)json_object_get_int(get(config,"max_inflight")))continue;
        if(own&&own->failed_until>now&&!strcmp(own->failed_account,id))continue;
        int priority=json_object_get_int(either(candidate,"Priority","priority"));
        int index=object_index(accounts,id);J *account=index<0?NULL:json_object_array_get_idx(accounts,(size_t)index);
        int fresh=deadline>now&&account&&json_object_get_int64(get(account,"valid_until"))>wall;
        int64_t reset=json_object_get_int64(get(account,"reset_at")),card=json_object_get_int64(get(account,"card_expiry")),urgency=INT64_MAX;
        if(fresh&&json_object_get_int(get(config,"prefer_reset"))&&reset>wall)urgency=reset;
        if(fresh&&card>wall&&card<urgency)urgency=card;
        int group=(int)groups[i];
        if(best<0||priority>best_priority||(priority==best_priority&&(group>best_group||(group==best_group&&(fresh>best_fresh||(fresh==best_fresh&&(urgency<best_deadline||(urgency==best_deadline&&loads[i]<best_load)))))))) {
            best=(int)i;best_priority=priority;best_group=group;best_fresh=fresh;best_deadline=urgency;best_load=loads[i];
        }
    }
    return best;
}
static void observe_project(const char *scope,const char *project,const char *name,time_t at) {
    if(!scope||!project||strlen(project)!=68||strncmp(project,"dir-",4))return;
    unsigned hash=2166136261u;
    for(const unsigned char *p=(const unsigned char *)project+4;*p;p++) {
        if(!(*p>='0'&&*p<='9')&&!(*p>='a'&&*p<='f'))return;
        hash=(hash^*p)*16777619u;
    }
    unsigned slot=hash%1024;
    if(!strcmp(observed_projects[slot].project,project)&&at<observed_projects[slot].seen+60)return;
    strcpy(observed_projects[slot].project,project);observed_projects[slot].seen=at;
    cwr_project_observed(project,name);
}
J *cwr_scheduler_pick(J *request) {
    J *result=json_object_new_object();pthread_mutex_lock(&lock);time_t now=monotonic(),wall=time(NULL);
    SessionKey key={0};char parent[LABEL]={0};
    const char *decision_reason=NULL,*selection=NULL,*scope=NULL,*project=NULL,*model=NULL;
    J *preference=NULL;unsigned chosen_load=0;
    J *config=get(snapshot,"config");int enabled=json_object_get_int(get(config,"enabled"));
    J *candidates=either(request,"Candidates","candidates"),*options=either(request,"Options","options");
    J *metadata=either(options,"Metadata","metadata"),*headers=either(options,"Headers","headers");
    if(!candidates||json_object_get_type(candidates)!=5||!length(candidates)||length(candidates)>MAX_ACCOUNTS)goto done;
    if(text(get(metadata,"source"))&&!strcmp(text(get(metadata,"source")),"plugin_host_model_callback"))goto done;
    if(json_object_get_type(get(metadata,"generate"))==1&&!json_object_get_int(get(metadata,"generate")))goto done;
    for(size_t i=0;i<length(candidates);i++) {
        const char *provider=text(either(json_object_array_get_idx(candidates,i),"Provider","provider"));
        if(!provider||strcmp(provider,"codex"))goto done;
        if(!text(either(json_object_array_get_idx(candidates,i),"ID","id"))){if(enabled)reject(result,"scheduler_unavailable");goto done;}
    }
    scope=text(get(metadata,"caller_scope"));project=header(headers,"X-CWR-Project");
    observe_project(scope,project,header(headers,"X-CWR-Project-Name"),now);
    /* A daemon outage stops quota optimization, not business routing. Keep
     * ownership of affinity and use the host's current available candidates. */
    model=text(get(metadata,"requested_model"));if(!model)model=text(either(request,"Model","model"));
    if(scope)strcpy(key.scope,scope);
    if(project)strcpy(key.project,project);
    if(model)strcpy(key.model,model);
    strcpy(key.provider,"codex");session_identity(metadata,headers,key.session,parent);
    selection=text(get(metadata,"selection_id"));
    if(!enabled){decision_reason="host_selected";goto done;}
    if(selection)for(unsigned i=0;i<SLOTS;i++)if(pending[i].expires>now&&!strcmp(pending[i].id,selection)) {
        int slot=pending[i].session_slot;
        int current=slot<0||(sessions[slot].expires>now&&sessions[slot].generation==pending[i].generation);
        if(current&&candidate_index(candidates,pending[i].account)>=0&&!held(pending[i].account)&&!forbidden(pending[i].account)) {
            json_object_object_add(result,"Handled",json_object_new_boolean(1));
            json_object_object_add(result,"AuthID",json_object_new_string(pending[i].account));decision_reason=pending[i].reason[0]?pending[i].reason:"available_pool";goto done;
        }
        pending[i].expires=0;
    }
    int own=find_session(&key,now),bound=own;
    if(own>=0&&sessions[own].account[0]&&invalid_binding(&sessions[own]))detach(&sessions[own],now,0);
    if((own<0||!sessions[own].detached)&&parent[0]&&boolean_default(config,"subagent_affinity",1)&&(own<0||!sessions[own].account[0]))bound=parent_binding(&key,parent,now);
    int chosen=-1,preserve=0,detached=own>=0&&sessions[own].detached;
    const char *pin=text(get(metadata,"pinned_auth_id"));
    if(pin) {
        chosen=candidate_index(candidates,pin);preserve=1;decision_reason="pinned";
        if(chosen<0||forbidden(pin)||held(pin)||inflight(pin,now)>=(unsigned)json_object_get_int(get(config,"max_inflight"))){reject(result,"scheduler_busy");goto done;}
    } else if(bound>=0&&sessions[bound].account[0]) {
        chosen=candidate_index(candidates,sessions[bound].account);
        if(chosen>=0)decision_reason=bound==own?"session_affinity":"parent_affinity";
        if(chosen>=0&&(held(sessions[bound].account)||inflight(sessions[bound].account,now)>=(unsigned)json_object_get_int(get(config,"max_inflight")))){reject(result,"scheduler_busy");goto done;}
        if(chosen<0) {
            const char *rebind=text(get(config,"rebind_on"));
            if(own>=0&&sessions[own].account[0]&&(!rebind||strcmp(rebind,"any")))preserve=1;else detached=1;
        }
    }
    J *projects=get(config,"projects");
    for(size_t i=0;i<length(projects);i++) {
        J *p=json_object_array_get_idx(projects,i);
        if(!strcmp(text(get(p,"project")),key.project)){preference=get(p,"accounts");break;}
    }
    if(chosen<0)chosen=choose_new(candidates,preference,own<0?NULL:&sessions[own],now,wall);
    if(chosen<0){reject(result,"scheduler_busy");goto done;}
    const char *account=text(either(json_object_array_get_idx(candidates,(size_t)chosen),"ID","id"));
    if(!account){reject(result,"scheduler_busy");goto done;}
    if(!decision_reason) {
        if(own>=0&&sessions[own].failed_until>now)decision_reason="failover";
        else {
            int preferred=0;for(size_t i=0;i<length(preference);i++)if(!strcmp(text(json_object_array_get_idx(preference,i)),account)){preferred=1;break;}
            int state_index=object_index(get(snapshot,"accounts"),account);J *state=state_index<0?NULL:json_object_array_get_idx(get(snapshot,"accounts"),(size_t)state_index);
            int fresh=deadline>now&&state&&json_object_get_int64(get(state,"valid_until"))>wall;
            decision_reason=preferred?"project_preference":fresh?"quota_load":"available_pool";
        }
    }
    chosen_load=inflight(account,now);
    unsigned reservation=SLOTS,occupied=0;
    for(unsigned i=0;i<SLOTS;i++) {
        if(pending[i].expires<=now)reservation=i;else occupied++;
        if(active[i].expires>now)occupied++;
    }
    if(reservation==SLOTS||occupied>=SLOTS){reject(result,"scheduler_busy");goto done;}
    /* Bind before exposing the choice so parallel first turns agree. */
    int slot=-1;
    if(!preserve)slot=bind_session(&key,account,now,json_object_get_int(get(config,"session_ttl_seconds")),detached);
    else if(own>=0&&!pin)sessions[own].expires=now+json_object_get_int(get(config,"session_ttl_seconds"));
    pending[reservation]=(Execution){.expires=now+15,.session_slot=slot,.generation=slot<0?0:sessions[slot].generation};
    if(selection)strcpy(pending[reservation].id,selection);
    strcpy(pending[reservation].account,account);
    snprintf(pending[reservation].reason,sizeof(pending[reservation].reason),"%s",decision_reason?decision_reason:"unknown");
    json_object_object_add(result,"Handled",json_object_new_boolean(1));
    json_object_object_add(result,"AuthID",json_object_new_string(account));
done:
    if(selection&&scope) {
        const char *picked=text(get(result,"AuthID"));int rejected=json_object_get_int(get(result,"Reject"));
        if(picked||rejected||(decision_reason&&!strcmp(decision_reason,"host_selected"))) {
            J *t=json_object_new_object(),*evidence=json_object_new_object();
            json_object_object_add(t,"phase",json_object_new_string("decision"));
            json_object_object_add(t,"selection_id",json_object_new_string(selection));
            json_object_object_add(t,"caller",json_object_new_string(scope));
            if(project)json_object_object_add(t,"project",json_object_new_string(project));
            if(model)json_object_object_add(t,"model",json_object_new_string(model));
            if(key.session[0])json_object_object_add(t,"session",json_object_new_string(key.session));
            if(parent[0])json_object_object_add(t,"parent_session",json_object_new_string(parent));
            if(picked)json_object_object_add(t,"account_id",json_object_new_string(picked));
            json_object_object_add(t,"reason",json_object_new_string(rejected?"scheduler_busy":decision_reason?decision_reason:"unknown"));
            json_object_object_add(t,"status",json_object_new_string(rejected?"not_sent":"selected"));
            json_object_object_add(evidence,"snapshot_fresh",json_object_new_boolean(deadline>now));
            json_object_object_add(evidence,"candidate_count",json_object_new_int64((int64_t)length(candidates)));
            if(picked)json_object_object_add(evidence,"inflight",json_object_new_int((int)chosen_load));
            if(get(config,"max_inflight"))json_object_object_add(evidence,"max_inflight",json_object_new_int(json_object_get_int(get(config,"max_inflight"))));
            json_object_object_add(t,"evidence",evidence);
            cwr_traffic_emit(selection,t);json_object_put(t);
        }
    }
    pthread_mutex_unlock(&lock);return reply(result);
}
