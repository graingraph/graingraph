#pragma once
#include <stdbool.h>
#include <stddef.h>
#include <stdint.h>

#ifdef __cplusplus
extern "C" {
#endif

typedef enum {
    JV_NULL,
    JV_BOOL,
    JV_INT64,
    JV_DOUBLE,
    JV_STRING,
    JV_ARRAY,
    JV_OBJECT
} jv_type;

typedef struct JsonValue JsonValue;

struct JsonValue;

typedef struct {
    uint8_t typ;
    union {
        int64_t i64;
        double dbl;
        int bool_val;
        char *str;
    } v;
} FlatValue;

int fast_decode_object(const char *s, size_t len,
                        char ***out_keys, struct JsonValue ***out_vals, size_t *out_count);

void fast_free_result(char **keys, JsonValue **vals, size_t count);

int fast_decode_object_flat(const char *s, size_t len,
                            char ***out_keys, FlatValue **out_vals, size_t *out_count);
int fast_decode_object_flat_from_bytes(const unsigned char *data, size_t len,
                                       char ***out_keys, FlatValue **out_vals, size_t *out_count);
void fast_free_flat(char **keys, FlatValue *vals, size_t count);

jv_type json_value_type(const JsonValue *v);
int json_value_is_primitive(const JsonValue *v);
int64_t json_value_int64(const JsonValue *v);
double json_value_double(const JsonValue *v);
int json_value_bool(const JsonValue *v);
const char *json_value_string(const JsonValue *v);
size_t json_value_string_len(const JsonValue *v);

#ifdef __cplusplus
}
#endif
