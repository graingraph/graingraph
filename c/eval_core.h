#pragma once
#include <stdbool.h>
#include <stddef.h>
#include <stdint.h>

#ifdef __cplusplus
extern "C" {
#endif

typedef enum {
    EV_NULL,
    EV_BOOL,
    EV_INT64,
    EV_DOUBLE,
    EV_STRING,
    EV_LIST,
    EV_MAP
} ev_type;

typedef struct {
    ev_type type;
    union {
        bool b;
        int64_t i;
        double d;
        const char *s;
        void *ptr;
    } v;
} ev_value;

typedef struct {
    const unsigned char *json;
    size_t json_len;
} ev_props;

int ev_eval_expr(const unsigned char *expr_data, size_t expr_len,
                 const ev_props *props, ev_value *out);

void ev_value_free(ev_value *v);

#ifdef __cplusplus
}
#endif
