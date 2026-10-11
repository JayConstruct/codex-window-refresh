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
static _Atomic int callbacks=0,model_calls=0,free_calls=0;
static void host_free(void*ptr,size_t len){(void)len;free_calls++;free(ptr);}
static int callback(void*ctx,const char*method,const uint8_t*req,size_t len,buffer*out){
 (void)req;(void)len;
 if(!ctx || *(int*)ctx!=73 || !out)return 1;
 callbacks++;
 if(strstr(method,"model")){model_calls++;return 1;}
 const char *json="{\"ok\":true,\"result\":{\"files\":[]}}";
 out->len=strlen(json);out->ptr=malloc(out->len);memcpy(out->ptr,json,out->len);return 0;
}
static int call(plugin_api*api,const char*method,const char*request,const char*expect){
 buffer out={0};int status=api->call((char*)method,(uint8_t*)request,request?strlen(request):0,&out);
 char*raw=calloc(out.len+1,1);if(out.len)memcpy(raw,out.ptr,out.len);
 int ok=status==0 && strstr(raw,expect)!=NULL;
 if(!ok)fprintf(stderr,"RPC %s failed: %s\n",method,raw);
 api->free_buffer(out.ptr,out.len);free(raw);return ok;
}
int main(int argc,char**argv){
 if(argc!=3){fprintf(stderr,"usage: fake_host plugin.so lifecycle.json\n");return 2;}
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
 free(register_request);
 for(int i=0;i<100 && atomic_load(&callbacks)==0;i++)usleep(10000);
 if(atomic_load(&callbacks)==0){fprintf(stderr,"runtime did not use copied host API\n");return 19;}
 if(!call(&api,"management.register","{}","/v0/management/codex-window-refresh/summary"))return 10;
 if(!call(&api,"management.handle","{\"Method\":\"GET\",\"Path\":\"/v0/management/codex-window-refresh/summary\"}","\"StatusCode\":200"))return 11;
 if(!call(&api,"request.intercept_before","{\"RequestID\":\"r\",\"Headers\":{\"X-Keep\":[\"preserved\"]},\"Body\":\"e30=\",\"Metadata\":{\"caller_scope\":\"test-scope\"}}","preserved"))return 12;
 buffer out={0};if(api.call(NULL,NULL,0,&out)==0)return 13;api.free_buffer(out.ptr,out.len);
 out=(buffer){0};if(api.call("request.complete",NULL,1,&out)==0)return 14;api.free_buffer(out.ptr,out.len);
 out=(buffer){0};if(api.call("request.complete",NULL,((size_t)64<<20)+1,&out)==0)return 15;api.free_buffer(out.ptr,out.len);
 if(!call(&api,"plugin.quiesce","{}","\"ok\":true"))return 16;
 api.shutdown();
 if(model_calls){fprintf(stderr,"unexpected model requests: %d\n",model_calls);return 17;}
 if(callbacks!=free_calls){fprintf(stderr,"host buffers leaked: calls=%d freed=%d\n",callbacks,free_calls);return 18;}
 // Go c-shared code is not hot-unloaded in this harness. Host shutdown drained
 // every callback; process exit releases the shared runtime mapping.
 printf("ABI 1 / schema 6 handshake passed; host_calls=%d buffers_freed=%d model_calls=0\n",callbacks,free_calls);
 return 0;
}
