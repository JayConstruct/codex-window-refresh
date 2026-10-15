// Test-only native ABI host. Never executes a model request or contacts a network.
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <dlfcn.h>
#include <unistd.h>
#include <stdatomic.h>

typedef struct {void *ptr;size_t len;} buffer;
typedef int (*host_call)(void*,const char*,const uint8_t*,size_t,buffer*);
typedef void (*free_buffer)(void*,size_t);
typedef struct {uint32_t abi_version;void *host_ctx;host_call call;free_buffer free_buffer;} host_api;
typedef int (*plugin_call)(char*,uint8_t*,size_t,buffer*);
typedef struct {uint32_t abi_version;plugin_call call;free_buffer free_buffer;void(*shutdown)(void);} plugin_api;
typedef int (*plugin_init)(host_api*,plugin_api*);
static _Atomic int callbacks=0,model_calls=0,free_calls=0,quota_calls=0;
static void host_free(void*ptr,size_t len){(void)len;free_calls++;free(ptr);}
static int callback(void*ctx,const char*method,const uint8_t*req,size_t len,buffer*out){
 if(!ctx || *(int*)ctx!=73 || !out)return 1;
 callbacks++;
 if(strstr(method,"model")){model_calls++;return 1;}
 const char *json="{\"ok\":true,\"result\":{}}";
 if(strcmp(method,"host.auth.list")==0)json="{\"ok\":true,\"result\":{\"files\":[{\"id\":\"test-auth\",\"auth_index\":\"test-index\",\"provider\":\"codex\",\"status\":\"active\"}]}}";
 else if(strcmp(method,"host.auth.get_runtime")==0)json="{\"ok\":true,\"result\":{\"auth\":{\"id\":\"test-auth\",\"auth_index\":\"test-index\",\"provider\":\"codex\",\"status\":\"active\"}}}";
 else if(strcmp(method,"host.auth.get")==0)json="{\"ok\":true,\"result\":{\"auth_index\":\"test-index\",\"json\":{\"type\":\"codex\",\"account_id\":\"test-upstream\",\"access_token\":\"native-test-token\"}}}";
 // Match MethodHostHTTPOperationOpen from the pinned official sdk/pluginabi/types.go.
 else if(strcmp(method,"host.http.operation_open")==0)json="{\"ok\":true,\"result\":{\"operation_id\":\"test-quota-operation\"}}";
 else if(strcmp(method,"host.http.do")==0){
   char *request=calloc(len+1,1);if(!request)return 1;if(len)memcpy(request,req,len);
   int bound=strstr(request,"https://chatgpt.com/backend-api/wham/usage") && strstr(request,"Bearer native-test-token") && strstr(request,"test-upstream") && strstr(request,"\"method\":\"GET\"") && strstr(request,"test-quota-operation");
   free(request);if(!bound)return 1;
   quota_calls++;
   json="{\"ok\":true,\"result\":{\"StatusCode\":200,\"Body\":\"eyJyYXRlX2xpbWl0Ijp7ImFsbG93ZWQiOnRydWV9fQ==\"}}";
 }
 else if(strcmp(method,"host.http.cancel")!=0)return 1;
 out->len=strlen(json);out->ptr=malloc(out->len);memcpy(out->ptr,json,out->len);return 0;
}
static int call_checked(plugin_api*api,const char*method,const char*request,const char*expect,int report){
 buffer out={0};int status=api->call((char*)method,(uint8_t*)request,request?strlen(request):0,&out);
 char*raw=calloc(out.len+1,1);if(out.len)memcpy(raw,out.ptr,out.len);
 int ok=status==0 && strstr(raw,expect)!=NULL;
 if(!ok && report)fprintf(stderr,"RPC %s failed: %s\n",method,raw);
 api->free_buffer(out.ptr,out.len);free(raw);return ok;
}
static int call(plugin_api*api,const char*method,const char*request,const char*expect){return call_checked(api,method,request,expect,1);}
static int report_has(const char*file,const char*phase,const char*library){
 FILE*f=fopen(file,"rb");if(!f)return 0;
 char raw[16385]={0};size_t size=fread(raw,1,16384,f);int extra=fgetc(f);fclose(f);
 if(!size || extra!=EOF)return 0;
 char expected_phase[64];snprintf(expected_phase,sizeof(expected_phase),"\"phase\":\"%s\"",phase);
 return strstr(raw,"\"hot_reload_protocol\":2") && strstr(raw,expected_phase) && strstr(raw,library)
   && strstr(raw,"\"reload_token\":\"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\"");
}
int main(int argc,char**argv){
 if(argc!=5){fprintf(stderr,"usage: fake_host plugin.so lifecycle.json replacement.so runtime-version.json\n");return 2;}
 void*lib=dlopen(argv[1],RTLD_NOW|RTLD_LOCAL);if(!lib){fprintf(stderr,"%s\n",dlerror());return 2;}
 plugin_init init=(plugin_init)dlsym(lib,"cliproxy_plugin_init");if(!init)return 3;
 plugin_api api={0};int context=73;host_api host={1,&context,callback,host_free};
 if(init(NULL,&api)==0 || init(&host,NULL)==0)return 4;
 host.abi_version=2;if(init(&host,&api)==0)return 5;host.abi_version=1;
 if(init(&host,&api)!=0 || api.abi_version!=1 || !api.call || !api.free_buffer || !api.shutdown)return 6;
 // The plugin must copy host_api; the original temporary struct is destroyed.
 memset(&host,0,sizeof(host));
 FILE*f=fopen(argv[2],"rb");if(!f)return 7;fseek(f,0,SEEK_END);long size=ftell(f);rewind(f);char*register_request=calloc((size_t)size+1,1);if(fread(register_request,1,(size_t)size,f)!=(size_t)size)return 8;fclose(f);
 if(!call(&api,"plugin.register",register_request,"\"schema_version\":6"))return 9;
 if(!report_has(argv[4],"running",argv[1]))return 43;
 for(int i=0;i<100 && atomic_load(&quota_calls)==0;i++)usleep(10000);
 if(atomic_load(&quota_calls)==0){fprintf(stderr,"native quota did not use copied host API\n");return 19;}
 // A network callback can finish before its account snapshot is committed.
 // Wait for the published account, not a guessed delay after the callback.
 int account_ready=0;
 for(int i=0;i<500 && !account_ready;i++){
   account_ready=call_checked(&api,"management.handle","{\"Method\":\"GET\",\"Path\":\"/v0/management/codex-window-refresh/accounts\"}","\"Body\":\"eyJ0ZXN0LWF1dGgi",0);
   if(!account_ready)usleep(10000);
 }
 if(!account_ready){fprintf(stderr,"native quota account snapshot was not published\n");return 25;}
 if(!call(&api,"management.register","{}","/v0/management/codex-window-refresh/summary"))return 10;
 if(!call(&api,"management.handle","{\"Method\":\"GET\",\"Path\":\"/v0/management/codex-window-refresh/summary\"}","\"StatusCode\":200"))return 11;
 int before_check=atomic_load(&quota_calls);
 if(!call(&api,"management.handle","{\"Method\":\"POST\",\"Path\":\"/v0/management/codex-window-refresh/quota-connection-check\"}","\"StatusCode\":200"))return 26;
 if(atomic_load(&quota_calls)!=before_check){fprintf(stderr,"local connection check issued HTTP\n");return 27;}
 int before=atomic_load(&quota_calls);
 if(!call(&api,"management.handle","{\"Method\":\"POST\",\"Path\":\"/v0/management/codex-window-refresh/quota-query\",\"Body\":\"eyJhY2NvdW50X2lkIjoidGVzdC1hdXRoIn0=\"}","\"StatusCode\":202"))return 23;
 for(int i=0;i<100 && atomic_load(&quota_calls)==before;i++)usleep(10000);
 if(atomic_load(&quota_calls)!=before+1){fprintf(stderr,"single-account native quota query missing or duplicated\n");return 24;}
 if(!call(&api,"request.intercept_before","{\"RequestID\":\"r\",\"Headers\":{\"X-Keep\":[\"preserved\"]},\"Body\":\"e30=\",\"Metadata\":{\"caller_scope\":\"test-scope\"}}","preserved"))return 12;
 // Response observation returns empty modifications and tolerates missing quota.
 if(!call(&api,"response.intercept_after","{\"RequestID\":\"r\",\"ResponseHeaders\":{},\"Body\":\"e30=\"}","\"result\":{}"))return 20;
 if(!call(&api,"response.intercept_stream_chunk","{\"RequestID\":\"r\",\"ChunkIndex\":-1}","\"result\":{}"))return 21;
 if(!call(&api,"websocket.response_event","{\"RequestID\":\"r\",\"Provider\":\"codex\",\"AuthID\":\"a\"}","\"result\":{}"))return 22;
 buffer out={0};if(api.call(NULL,NULL,0,&out)==0)return 13;api.free_buffer(out.ptr,out.len);
 out=(buffer){0};if(api.call("request.complete",NULL,1,&out)==0)return 14;api.free_buffer(out.ptr,out.len);
 out=(buffer){0};if(api.call("request.complete",NULL,((size_t)64<<20)+1,&out)==0)return 15;api.free_buffer(out.ptr,out.len);
 if(!call(&api,"plugin.quiesce","{}","\"ok\":true"))return 16;
 if(!report_has(argv[4],"quiesced",argv[1]))return 44;
 if(!call(&api,"plugin.quiesce","{}","\"ok\":true"))return 28;
 if(!call(&api,"plugin.reconfigure",register_request,"\"schema_version\":6"))return 29;
 void*replacement=dlopen(argv[3],RTLD_NOW|RTLD_LOCAL);if(!replacement)return 30;
 plugin_init init_next=(plugin_init)dlsym(replacement,"cliproxy_plugin_init");
 plugin_api next={0};host=(host_api){1,&context,callback,host_free};
 if(!init_next || init_next(&host,&next)!=0)return 31;
 // A replacement must fail while the old version owns the durable state lock.
 if(!call(&next,"plugin.register",register_request,"state store initialization failed"))return 32;
 next.shutdown();
 if(init_next(&host,&next)!=0)return 33;
 if(!call(&api,"plugin.quiesce","{}","\"ok\":true"))return 34;
 if(!call(&next,"plugin.register",register_request,"\"schema_version\":6"))return 35;
 if(!report_has(argv[4],"running",argv[3]))return 45;
 if(!call(&api,"plugin.reconfigure",register_request,"state store initialization failed"))return 36;
 if(!report_has(argv[4],"running",argv[3]))return 46;
 // Replacement failure cleanup releases its lock before CPA resumes the old version.
 next.shutdown();
 if(!report_has(argv[4],"stopped",argv[3]))return 47;
 if(dlclose(replacement)!=0)return 37;
 if(!call(&api,"plugin.reconfigure",register_request,"\"schema_version\":6"))return 38;
 api.shutdown();
 if(dlclose(lib)!=0)return 39;
 // RTLD_NODELETE retains Go executable mappings; reopening the same path must
 // nevertheless create a fresh service and acquire the released state lock.
 lib=dlopen(argv[1],RTLD_NOW|RTLD_LOCAL);if(!lib)return 40;
 init=(plugin_init)dlsym(lib,"cliproxy_plugin_init");
 if(!init || init(&host,&api)!=0)return 41;
 if(!call(&api,"plugin.register",register_request,"\"schema_version\":6"))return 42;
 api.shutdown();dlclose(lib);free(register_request);
 if(!report_has(argv[4],"stopped",argv[1]))return 48;
 if(model_calls){fprintf(stderr,"unexpected model requests: %d\n",model_calls);return 17;}
 if(callbacks!=free_calls){fprintf(stderr,"host buffers leaked: calls=%d freed=%d\n",callbacks,free_calls);return 18;}
 printf("ABI 1 / schema 6 hot reload, local version receipt, lock handoff, rollback and reopen passed; host_calls=%d buffers_freed=%d quota_queries=%d model_calls=0\n",callbacks,free_calls,quota_calls);
 return 0;
}
