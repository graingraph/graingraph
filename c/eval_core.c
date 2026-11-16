#include "eval_core.h"
#include <stdlib.h>
#include <string.h>

static void ev_value_set_null(ev_value *v) { v->type = EV_NULL; }
static void ev_value_set_bool(ev_value *v, bool b) { v->type = EV_BOOL; v->v.b = b; }
static void ev_value_set_int(ev_value *v, int64_t i) { v->type = EV_INT64; v->v.i = i; }
static void ev_value_set_double(ev_value *v, double d) { v->type = EV_DOUBLE; v->v.d = d; }

void ev_value_free(ev_value *v) {
    if (!v) return;
    if (v->type == EV_STRING) {
        free((void*)v->v.s);
    } else if (v->type == EV_LIST || v->type == EV_MAP) {
        free(v->v.ptr);
    }
    v->type = EV_NULL;
}

// Minimal stub: always return NULL. Real walker will be filled in.
int ev_eval_expr(const unsigned char *expr_data, size_t expr_len,
                 const ev_props *props, ev_value *out) {
    if (!expr_data || !out) return 0;
    ev_value_set_null(out);
    return 1;
}
