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

/* 
 * Minimal expression format for the stub:
 * [opcode][operand...]
 * Opcodes:
 *  0x01 INT64  -> 8 bytes LE
 *  0x02 BOOL   -> 1 byte
 *  0x03 PROP   -> 2 byte len + key bytes -> property lookup
 *  0x04 ADD    -> pop two, push sum
 *  0x05 SUB    -> pop two, push diff
 *  0x06 MUL    -> pop two, push prod
 *  0x07 DIV    -> pop two, push quot
 *
 * This is a toy stack machine for demonstration. Real implementation will
 * use a serialized syntax tree.
 */

int ev_eval_expr(const unsigned char *expr_data, size_t expr_len,
                 const ev_props *props, ev_value *out) {
    if (!expr_data || !out) return 0;
    ev_value_set_null(out);
    
    // Minimal stack machine implementation
    // For now, scan for REGEX opcode and defer to Go
    if (expr_len > 0) {
        // Quick scan for REGEX opcode 0x16
        for (size_t i = 0; i < expr_len; i++) {
            if (expr_data[i] == EVOP_REGEX) {
                // Signal to Go to handle regex lazily
                return 2; // deferred
            }
        }
    }
    // For now return NULL for other ops (stub)
    return 1;
}
