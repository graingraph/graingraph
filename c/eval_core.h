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
    EV_MAP,
    EV_TEMPORAL,
    EV_POINT
} ev_type;

typedef enum {
    EVOP_INT64 = 0x01,
    EVOP_BOOL  = 0x02,
    EVOP_PROP  = 0x03,
    EVOP_ADD   = 0x04,
    EVOP_SUB   = 0x05,
    EVOP_MUL   = 0x06,
    EVOP_DIV   = 0x07,
    EVOP_EQ    = 0x08,
    EVOP_NEQ   = 0x09,
    EVOP_LT    = 0x0A,
    EVOP_GT    = 0x0B,
    EVOP_LTE   = 0x0C,
    EVOP_GTE   = 0x0D,
    EVOP_AND   = 0x0E,
    EVOP_OR    = 0x0F,
    EVOP_NOT   = 0x10,
    EVOP_IN    = 0x11,
    EVOP_ISNULL= 0x12,
    EVOP_STARTS= 0x13,
    EVOP_CONTAINS=0x14,
    EVOP_ENDS  = 0x15,
    EVOP_REGEX = 0x16,
    EVOP_TEMP_ADD = 0x17,
    EVOP_TEMP_SUB = 0x18,
    EVOP_POINT_EQ=0x19,
    EVOP_POINT_DIST=0x1A
} ev_opcode;

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
