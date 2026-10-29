/* Narrow public json-c ABI declarations. Link the system json-c SONAME 5.
 * No json-c implementation is copied. API provenance: json-c 0.16 public
 * json_object.h and json_tokener.h. Debian 12/13 provide this ABI. */
#ifndef CWR_JSON_API_H
#define CWR_JSON_API_H
#include <stddef.h>
#include <stdint.h>
typedef struct json_object J;
struct json_tokener;
extern J *json_object_new_object(void);
extern J *json_object_new_array(void);
extern J *json_object_new_string(const char *);
extern J *json_object_new_boolean(int);
extern J *json_object_new_int(int);
extern J *json_object_new_int64(int64_t);
extern J *json_object_get(J *);
extern int json_object_put(J *);
extern int json_object_object_add(J *,const char *,J *);
extern void json_object_object_del(J *,const char *);
extern int json_object_object_get_ex(const J *,const char *,J **);
extern int json_object_object_length(const J *);
extern int json_object_array_add(J *,J *);
extern size_t json_object_array_length(const J *);
extern J *json_object_array_get_idx(const J *,size_t);
extern const char *json_object_get_string(J *);
extern int json_object_get_string_len(const J *);
extern int json_object_get_type(const J *);
extern int json_object_get_int(const J *);
/* Public iterator ABI, json-c 0.16 json_object_iterator.h. */
struct json_object_iterator { const void *opaque_; };
extern struct json_object_iterator json_object_iter_begin(J *);
extern struct json_object_iterator json_object_iter_end(const J *);
extern int json_object_iter_equal(const struct json_object_iterator *,const struct json_object_iterator *);
extern void json_object_iter_next(struct json_object_iterator *);
extern const char *json_object_iter_peek_name(const struct json_object_iterator *);
extern J *json_object_iter_peek_value(const struct json_object_iterator *);
extern int64_t json_object_get_int64(const J *);
extern const char *json_object_to_json_string_ext(J *,int);
extern struct json_tokener *json_tokener_new_ex(int);
extern void json_tokener_free(struct json_tokener *);
extern void json_tokener_set_flags(struct json_tokener *,int);
extern J *json_tokener_parse_ex(struct json_tokener *,const char *,int);
extern int json_tokener_get_error(struct json_tokener *);
extern size_t json_tokener_get_parse_end(struct json_tokener *);
#endif
