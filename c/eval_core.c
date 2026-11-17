#include "eval_core.h"
#include <stdlib.h>
#include <string.h>

static void ev_value_set_null(ev_value *v) { v->typ = EV_NULL; }
static void ev_value_set_bool(ev_value *v, bool b) { v->typ = EV_BOOL; v->v.b = b; }
static void ev_value_set_int(ev_value *v, int64_t i) { v->typ = EV_INT64; v->v.i = i; }
static void ev_value_set_double(ev_value *v, double d) { v->typ = EV_DOUBLE; v->v.d = d; }

void ev_value_free(ev_value *v) {
    if (!v) return;
    if (v->typ == EV_STRING) {
        free((void*)v->v.s);
    } else if (v->typ == EV_LIST || v->typ == EV_MAP) {
        free(v->v.ptr);
    }
    v->typ = EV_NULL;
}

#define STACK_MAX 256

static void ev_push(ev_value *stack, int *sp, ev_value v) {
    if (*sp < STACK_MAX) {
        stack[*sp] = v;
        (*sp)++;
    }
}
static ev_value ev_pop(ev_value *stack, int *sp) {
    ev_value v = {0};
    if (*sp > 0) {
        (*sp)--;
        v = stack[*sp];
    }
    return v;
}

static int64_t read_i64(const unsigned char *p) {
    int64_t v = 0;
    for (int i = 0; i < 8; i++) {
        v |= ((int64_t)p[i]) << (i * 8);
    }
    return v;
}
static double read_double(const unsigned char *p) {
    double d;
    memcpy(&d, p, sizeof(double));
    return d;
}

int ev_eval_expr(const unsigned char *expr_data, size_t expr_len,
                 const ev_props *props, ev_value *out) {
    if (!expr_data || !out) return 0;
    ev_value_set_null(out);

    // Quick scan for regex defer
    for (size_t i = 0; i < expr_len; i++) {
        if (expr_data[i] == EVOP_REGEX) {
            return 2; // defer to Go
        }
    }

    ev_value stack[STACK_MAX];
    int sp = 0;
    size_t pc = 0;

    while (pc < expr_len) {
        unsigned char op = expr_data[pc++];
        switch (op) {
            case EVOP_INT64: {
                if (pc + 8 > expr_len) return 0;
                int64_t v = read_i64(expr_data + pc);
                pc += 8;
                ev_value val; val.typ = EV_INT64; val.v.i = v;
                ev_push(stack, &sp, val);
                break;
            }
            case EVOP_BOOL: {
                if (pc >= expr_len) return 0;
                bool b = expr_data[pc++] != 0;
                ev_value val; val.typ = EV_BOOL; val.v.b = b;
                ev_push(stack, &sp, val);
                break;
            }
            case EVOP_STRING: {
                if (pc + 2 > expr_len) return 0;
                uint16_t len = (expr_data[pc] << 8) | expr_data[pc+1];
                pc += 2;
                if (pc + len > expr_len) return 0;
                char *s = malloc(len + 1);
                if (!s) return 0;
                memcpy(s, expr_data + pc, len);
                s[len] = '\0';
                ev_value val; val.typ = EV_STRING; val.v.s = s;
                ev_push(stack, &sp, val);
                pc += len;
                break;
            }
            case EVOP_PROP: {
                if (pc + 2 > expr_len) return 0;
                uint16_t len = (expr_data[pc] << 8) | expr_data[pc+1];
                pc += 2;
                if (pc + len > expr_len) return 0;
                // Build key string from bytes (no null terminator)
                char key[256];
                if (len >= sizeof(key)) return 0;
                memcpy(key, expr_data + pc, len);
                key[len] = '\0';
                ev_value val; val.typ = EV_NULL;
                if (props && props->keys && props->vals) {
                    for (size_t i = 0; i < props->count; i++) {
                        const char *k = props->keys[i];
                        if (k && strcmp(k, key) == 0) {
                            const unsigned char *vptr = (const unsigned char *)props->vals + i * 16;
                            uint8_t typ = vptr[0];
                            val.typ = EV_NULL;
                            if (typ == 0) {
                                val.typ = EV_NULL;
                            } else if (typ == 1) {
                                val.typ = EV_BOOL;
                                int32_t b = *(int32_t *)(vptr + 8);
                                val.v.b = b != 0;
                            } else if (typ == 2) {
                                val.typ = EV_INT64;
                                val.v.i = *(int64_t *)(vptr + 8);
                            } else if (typ == 3) {
                                val.typ = EV_DOUBLE;
                                val.v.d = *(double *)(vptr + 8);
                            } else if (typ == 4) {
                                val.typ = EV_STRING;
                                // FlatValue v.str is pointer at offset 8
                                val.v.s = *(const char **)(vptr + 8);
                            }
                            ev_push(stack, &sp, val);
                            pc += len;
                            goto prop_done;
                        }
                    }
                }
                ev_value val2; val2.typ = EV_NULL;
                ev_push(stack, &sp, val2);
                pc += len;
                break;
prop_done:
                break;
            }
            case EVOP_ADD:
            case EVOP_SUB:
            case EVOP_MUL:
            case EVOP_DIV: {
                if (sp < 2) return 0;
                ev_value b = ev_pop(stack, &sp);
                ev_value a = ev_pop(stack, &sp);
                ev_value res; res.typ = EV_NULL;
                if (a.typ == EV_INT64 && b.typ == EV_INT64) {
                    int64_t av = a.v.i, bv = b.v.i;
                    int64_t rv = 0;
                    if (op == EVOP_ADD) rv = av + bv;
                    else if (op == EVOP_SUB) rv = av - bv;
                    else if (op == EVOP_MUL) rv = av * bv;
                    else if (op == EVOP_DIV && bv != 0) rv = av / bv;
                    res.typ = EV_INT64; res.v.i = rv;
                } else {
                    res.typ = EV_DOUBLE;
                    double av = (a.typ == EV_INT64) ? (double)a.v.i : 0;
                    double bv = (b.typ == EV_INT64) ? (double)b.v.i : 0;
                    double rv = 0;
                    if (op == EVOP_ADD) rv = av + bv;
                    else if (op == EVOP_SUB) rv = av - bv;
                    else if (op == EVOP_MUL) rv = av * bv;
                    else if (op == EVOP_DIV && bv != 0.0) rv = av / bv;
                    res.v.d = rv;
                }
                ev_push(stack, &sp, res);
                break;
            }
            case EVOP_EQ:
            case EVOP_NEQ:
            case EVOP_LT:
            case EVOP_GT:
            case EVOP_LTE:
            case EVOP_GTE: {
                if (sp < 2) return 0;
                ev_value b = ev_pop(stack, &sp);
                ev_value a = ev_pop(stack, &sp);
                bool eq = false;
                if (a.typ == EV_INT64 && b.typ == EV_INT64) {
                    eq = a.v.i == b.v.i;
                }
                bool resb = false;
                switch (op) {
                    case EVOP_EQ:  resb = (a.typ == b.typ) && (a.typ == EV_INT64 ? a.v.i == b.v.i : false); break;
                    case EVOP_NEQ: resb = !(a.typ == b.typ && a.typ == EV_INT64 && a.v.i == b.v.i); break;
                    case EVOP_LT:  resb = a.typ == EV_INT64 && b.typ == EV_INT64 && a.v.i < b.v.i; break;
                    case EVOP_GT:  resb = a.typ == EV_INT64 && b.typ == EV_INT64 && a.v.i > b.v.i; break;
                    case EVOP_LTE: resb = a.typ == EV_INT64 && b.typ == EV_INT64 && a.v.i <= b.v.i; break;
                    case EVOP_GTE: resb = a.typ == EV_INT64 && b.typ == EV_INT64 && a.v.i >= b.v.i; break;
                }
                ev_value res; res.typ = EV_BOOL; res.v.b = resb;
                ev_push(stack, &sp, res);
                break;
            }
            case EVOP_AND:
            case EVOP_OR: {
                if (sp < 2) return 0;
                ev_value b = ev_pop(stack, &sp);
                ev_value a = ev_pop(stack, &sp);
                bool av = a.typ == EV_BOOL && a.v.b;
                bool bv = b.typ == EV_BOOL && b.v.b;
                ev_value res; res.typ = EV_BOOL;
                res.v.b = (op == EVOP_AND) ? (av && bv) : (av || bv);
                ev_push(stack, &sp, res);
                break;
            }
            case EVOP_NOT: {
                if (sp < 1) return 0;
                ev_value a = ev_pop(stack, &sp);
                ev_value res; res.typ = EV_BOOL; res.v.b = !(a.typ == EV_BOOL && a.v.b);
                ev_push(stack, &sp, res);
                break;
            }
            case EVOP_ISNULL: {
                if (sp < 1) return 0;
                ev_value a = ev_pop(stack, &sp);
                ev_value res; res.typ = EV_BOOL; res.v.b = (a.typ == EV_NULL);
                ev_push(stack, &sp, res);
                break;
            }
            case EVOP_TEMP_ADD:
            case EVOP_TEMP_SUB:
            case EVOP_IN: {
                if (sp < 2) return 0;
                ev_value b = ev_pop(stack, &sp);
                ev_value a = ev_pop(stack, &sp);
                bool eq = false;
                if (a.typ == EV_INT64 && b.typ == EV_INT64) {
                    eq = a.v.i == b.v.i;
                }
                ev_value res; res.typ = EV_BOOL; res.v.b = eq;
                ev_push(stack, &sp, res);
                break;
            }
            case EVOP_STARTS: {
                if (sp < 2) return 0;
                ev_value b = ev_pop(stack, &sp);
                ev_value a = ev_pop(stack, &sp);
                bool resb = false;
                if (a.typ == EV_STRING && b.typ == EV_STRING && a.v.s && b.v.s) {
                    size_t al = strlen(a.v.s);
                    size_t bl = strlen(b.v.s);
                    if (bl <= al) {
                        resb = memcmp(a.v.s, b.v.s, bl) == 0;
                    }
                }
                ev_value res; res.typ = EV_BOOL; res.v.b = resb;
                ev_push(stack, &sp, res);
                break;
            }
            case EVOP_CONTAINS: {
                if (sp < 2) return 0;
                ev_value b = ev_pop(stack, &sp);
                ev_value a = ev_pop(stack, &sp);
                bool resb = false;
                if (a.typ == EV_STRING && b.typ == EV_STRING && a.v.s && b.v.s) {
                    resb = strstr(a.v.s, b.v.s) != NULL;
                }
                ev_value res; res.typ = EV_BOOL; res.v.b = resb;
                ev_push(stack, &sp, res);
                break;
            }
            case EVOP_ENDS: {
                if (sp < 2) return 0;
                ev_value b = ev_pop(stack, &sp);
                ev_value a = ev_pop(stack, &sp);
                bool resb = false;
                if (a.typ == EV_STRING && b.typ == EV_STRING && a.v.s && b.v.s) {
                    size_t al = strlen(a.v.s);
                    size_t bl = strlen(b.v.s);
                    if (bl <= al) {
                        resb = memcmp(a.v.s + al - bl, b.v.s, bl) == 0;
                    }
                }
                ev_value res; res.typ = EV_BOOL; res.v.b = resb;
                ev_push(stack, &sp, res);
                break;
            }
            default:
                // Unknown opcode, stop
                return 0;
        }
    }

    if (sp > 0) {
        *out = stack[sp - 1];
    } else {
        ev_value_set_null(out);
    }
    return 1;
}
