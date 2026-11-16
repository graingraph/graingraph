#include "fastjson.h"
#include <stdlib.h>
#include <string.h>
#include <ctype.h>
#include <stdint.h>
#include <stdio.h>

#define MAX_FAST_DEPTH 64

typedef struct JsonValue JsonValue;
typedef struct JsonObject JsonObject;
typedef struct JsonArray JsonArray;

struct JsonValue {
    jv_type type;
    union {
        int64_t i64;
        double dbl;
        int bool_val;
        char *str;
        JsonArray *arr;
        JsonObject *obj;
    } as;
};

struct JsonObject {
    size_t count;
    char **keys;
    JsonValue **vals;
};

struct JsonArray {
    size_t count;
    JsonValue **items;
};

typedef struct {
    const char *s;
    size_t len;
    size_t i;
    int depth;
} fast_decoder;

static void ws_skip(fast_decoder *d) {
    while (d->i < d->len) {
        char c = d->s[d->i];
        if (c==' '||c=='\t'||c=='\n'||c=='\r') d->i++;
        else break;
    }
}

static int is_digit(char c){ return c>='0' && c<='9'; }

static JsonValue* json_value_new(jv_type t){ JsonValue *v=malloc(sizeof(JsonValue)); v->type=t; return v; }

static void json_value_free(JsonValue *v);
static void json_object_free(JsonObject *o);
static void json_array_free(JsonArray *a);

static void json_value_free(JsonValue *v){
    if(!v) return;
    switch(v->type){
        case JV_STRING: free(v->as.str); break;
        case JV_ARRAY: {
            JsonArray *a=v->as.arr;
            for(size_t i=0;i<a->count;i++) json_value_free(a->items[i]);
            free(a->items); free(a);
            break;
        }
        case JV_OBJECT: {
            JsonObject *o=v->as.obj;
            for(size_t i=0;i<o->count;i++){ free(o->keys[i]); json_value_free(o->vals[i]); }
            free(o->keys); free(o->vals); free(o);
            break;
        }
        default: break;
    }
    free(v);
}

static char* strndup_safe(const char *s, size_t n){
    char *p=malloc(n+1); memcpy(p,s,n); p[n]=0; return p;
}

static int parse_string(fast_decoder *d, char **out){
    if(d->i>=d->len || d->s[d->i]!='"') return 0;
    size_t start=d->i+1;
    d->i++;
    // check for escapes quickly
    size_t j=start;
    while(j<d->len){
        char c=d->s[j];
        if(c=='"'){ d->i=j+1; // ensure no escapes
          // check for backslash in between
          int has_escape=0;
          for(size_t k=start;k<j;k++) if(d->s[k]=='\\'){ has_escape=1; break; }
          if(has_escape){ return 0; } // fallback to Go
          *out=strndup_safe(d->s+start, j-start);
          return 1;
        }
        if(c=='\\'){ return 0; } // fallback
        j++;
    }
    return 0;
}

static int parse_number(fast_decoder *d, JsonValue **out){
    size_t start=d->i;
    int has_dot=0, has_exp=0;
    while(d->i<d->len){
        char c=d->s[d->i];
        if(is_digit(c) || c=='-' || c=='+' ){
            d->i++;
            continue;
        }
        if(c=='.'){ has_dot=1; d->i++; continue; }
        if(c=='e'||c=='E'){ has_exp=1; d->i++; // consume e and sign
            if(d->i<d->len && (d->s[d->i]=='-'||d->s[d->i]=='+')) d->i++;
            continue;
        }
        break;
    }
    size_t n=d->i-start;
    char *buf=strndup_safe(d->s+start, n);
    JsonValue *v=json_value_new(has_dot||has_exp? JV_DOUBLE: JV_INT64);
    if(has_dot||has_exp){
        v->as.dbl=strtod(buf,NULL);
    }else{
        v->as.i64=strtoll(buf,NULL,10);
    }
    free(buf);
    *out=v;
    return 1;
}

static int parse_value(fast_decoder *d, JsonValue **out);
static int parse_object(fast_decoder *d, JsonObject **out);
static int parse_array(fast_decoder *d, JsonArray **out);

static int parse_value(fast_decoder *d, JsonValue **out){
    ws_skip(d);
    if(d->i>=d->len) return 0;
    char c=d->s[d->i];
    if(c=='{'){
        JsonObject *o; if(!parse_object(d,&o)) return 0;
        JsonValue *v=json_value_new(JV_OBJECT); v->as.obj=o; *out=v; return 1;
    }
    if(c=='['){
        JsonArray *a; if(!parse_array(d,&a)) return 0;
        JsonValue *v=json_value_new(JV_ARRAY); v->as.arr=a; *out=v; return 1;
    }
    if(c=='"'){
        char *s; if(!parse_string(d,&s)) return 0;
        JsonValue *v=json_value_new(JV_STRING); v->as.str=s; *out=v; return 1;
    }
    if(c=='t' && d->i+3<d->len && strncmp(d->s+d->i,"true",4)==0){ d->i+=4; JsonValue *v=json_value_new(JV_BOOL); v->as.bool_val=1; *out=v; return 1; }
    if(c=='f' && d->i+4<d->len && strncmp(d->s+d->i,"false",5)==0){ d->i+=5; JsonValue *v=json_value_new(JV_BOOL); v->as.bool_val=0; *out=v; return 1; }
    if(c=='n' && d->i+3<d->len && strncmp(d->s+d->i,"null",4)==0){ d->i+=4; JsonValue *v=json_value_new(JV_NULL); *out=v; return 1; }
    if(c=='-' || is_digit(c)){ JsonValue *v; if(!parse_number(d,&v)) return 0; *out=v; return 1; }
    return 0;
}

static int parse_object(fast_decoder *d, JsonObject **out){
    if(d->s[d->i]!='{') return 0;
    d->i++; d->depth++;
    if(d->depth>MAX_FAST_DEPTH) return 0;
    ws_skip(d);
    JsonObject *o=calloc(1,sizeof(JsonObject));
    if(d->i<d->len && d->s[d->i]=='}'){ d->i++; d->depth--; *out=o; return 1; }
    size_t cap=4;
    o->keys=malloc(cap*sizeof(char*));
    o->vals=malloc(cap*sizeof(JsonValue*));
    while(1){
        char *key; if(!parse_string(d,&key)) goto fail;
        ws_skip(d);
        if(d->i>=d->len || d->s[d->i]!=':') { free(key); goto fail; }
        d->i++;
        JsonValue *val; if(!parse_value(d,&val)) { free(key); goto fail; }
        if(o->count>=cap){ cap*=2; o->keys=realloc(o->keys,cap*sizeof(char*)); o->vals=realloc(o->vals,cap*sizeof(JsonValue*)); }
        o->keys[o->count]=key;
        o->vals[o->count]=val;
        o->count++;
        ws_skip(d);
        if(d->i>=d->len) goto fail;
        if(d->s[d->i]==','){ d->i++; continue; }
        if(d->s[d->i]=='}'){ d->i++; d->depth--; *out=o; return 1; }
        goto fail;
    }
fail:
    json_object_free(o); return 0;
}

static int parse_array(fast_decoder *d, JsonArray **out){
    if(d->s[d->i]!='[') return 0;
    d->i++; d->depth++;
    if(d->depth>MAX_FAST_DEPTH) return 0;
    ws_skip(d);
    JsonArray *a=calloc(1,sizeof(JsonArray));
    size_t cap=4;
    a->items=malloc(cap*sizeof(JsonValue*));
    if(d->i<d->len && d->s[d->i]==']'){ d->i++; d->depth--; *out=a; return 1; }
    while(1){
        JsonValue *v; if(!parse_value(d,&v)) goto fail;
        if(a->count>=cap){ cap*=2; a->items=realloc(a->items,cap*sizeof(JsonValue*)); }
        a->items[a->count]=v;
        a->count++;
        ws_skip(d);
        if(d->i>=d->len) goto fail;
        if(d->s[d->i]==','){ d->i++; continue; }
        if(d->s[d->i]==']'){ d->i++; d->depth--; *out=a; return 1; }
        goto fail;
    }
fail:
    json_array_free(a); return 0;
}

static void json_object_free(JsonObject *o){
    if(!o) return;
    for(size_t i=0;i<o->count;i++){ free(o->keys[i]); json_value_free(o->vals[i]); }
    free(o->keys); free(o->vals); free(o);
}
static void json_array_free(JsonArray *a){
    if(!a) return;
    for(size_t i=0;i<a->count;i++) json_value_free(a->items[i]);
    free(a->items); free(a);
}

/* Public API: expose as key/value pairs for Go conversion */
int fast_decode_object(const char *s, size_t len,
                        char ***out_keys, JsonValue ***out_vals, size_t *out_count){
    fast_decoder d={s,len,0,0};
    ws_skip(&d);
    JsonObject *obj;
    if(!parse_object(&d,&obj)) return 0;
    ws_skip(&d);
    if(d.i!=len){ json_object_free(obj); return 0; }
    *out_count=obj->count;
    *out_keys=malloc(obj->count*sizeof(char*));
    *out_vals=malloc(obj->count*sizeof(JsonValue*));
    for(size_t i=0;i<obj->count;i++){
        (*out_keys)[i]=obj->keys[i];
        (*out_vals)[i]=obj->vals[i];
    }
    free(obj->keys); free(obj->vals); free(obj);
    return 1;
}

int fast_decode_object_flat(const char *s, size_t len,
                            char ***out_keys, FlatValue **out_vals, size_t *out_count){
    fast_decoder d={s,len,0,0};
    ws_skip(&d);
    JsonObject *obj;
    if(!parse_object(&d,&obj)) return 0;
    ws_skip(&d);
    if(d.i!=len){ json_object_free(obj); return 0; }
    // check all values are primitive
    for(size_t i=0;i<obj->count;i++){
        jv_type t=obj->vals[i]->type;
        if(t!=JV_NULL && t!=JV_BOOL && t!=JV_INT64 && t!=JV_DOUBLE && t!=JV_STRING){
            json_object_free(obj);
            return 0;
        }
    }
    *out_count=obj->count;
    *out_keys=malloc(obj->count*sizeof(char*));
    *out_vals=malloc(obj->count*sizeof(FlatValue));
    for(size_t i=0;i<obj->count;i++){
        (*out_keys)[i]=strndup_safe(obj->keys[i], strlen(obj->keys[i]));
        JsonValue *v=obj->vals[i];
        FlatValue *fv=&(*out_vals)[i];
        fv->typ=(uint8_t)v->type;
        switch(v->type){
            case JV_NULL: break;
            case JV_BOOL: fv->v.bool_val=v->as.bool_val; break;
            case JV_INT64: fv->v.i64=v->as.i64; break;
            case JV_DOUBLE: fv->v.dbl=v->as.dbl; break;
            case JV_STRING: fv->v.str=strndup_safe(v->as.str, strlen(v->as.str)); break;
            default: break;
        }
    }
    // free original object
    for(size_t i=0;i<obj->count;i++){
        free(obj->keys[i]);
        json_value_free(obj->vals[i]);
    }
    free(obj->keys); free(obj->vals); free(obj);
    return 1;
}

int fast_decode_object_flat_from_bytes(const unsigned char *data, size_t len,
                                       char ***out_keys, FlatValue **out_vals, size_t *out_count){
    // reinterpret as char *
    const char *s = (const char*)data;
    return fast_decode_object_flat(s, len, out_keys, out_vals, out_count);
}

int fast_decode_batch_flat(const unsigned char * const *datas, const size_t *lens, size_t count,
                           char ***out_keys, FlatValue **out_vals, size_t *out_counts){
    if(!datas || !lens || !out_keys || !out_vals || !out_counts || count==0) return 0;
    for(size_t i=0;i<count;i++){
        char **k = NULL;
        FlatValue *v = NULL;
        size_t c = 0;
        if(!fast_decode_object_flat_from_bytes(datas[i], lens[i], &k, &v, &c)){
            // cleanup already decoded
            for(size_t j=0;j<i;j++){
                fast_free_flat(out_keys[j], out_vals[j], out_counts[j]);
            }
            return 0;
        }
        out_keys[i] = k;
        out_vals[i] = v;
        out_counts[i] = c;
    }
    return 1;
}

void fast_free_result(char **keys, JsonValue **vals, size_t count){
    for(size_t i=0;i<count;i++){
        free(keys[i]);
        json_value_free(vals[i]);
    }
    free(keys);
    free(vals);
}

void fast_free_flat(char **keys, FlatValue *vals, size_t count){
    for(size_t i=0;i<count;i++){
        free(keys[i]);
        if(vals[i].typ==JV_STRING){
            free(vals[i].v.str);
        }
    }
    free(keys);
    free(vals);
}

jv_type json_value_type(const JsonValue *v){ return v? v->type: JV_NULL; }
int json_value_is_primitive(const JsonValue *v){
    if(!v) return 0;
    return v->type==JV_NULL||v->type==JV_BOOL||v->type==JV_INT64||v->type==JV_DOUBLE||v->type==JV_STRING;
}
int64_t json_value_int64(const JsonValue *v){ return v? v->as.i64: 0; }
double json_value_double(const JsonValue *v){ return v? v->as.dbl: 0; }
int json_value_bool(const JsonValue *v){ return v? v->as.bool_val: 0; }
const char *json_value_string(const JsonValue *v){ return v? v->as.str: NULL; }
size_t json_value_string_len(const JsonValue *v){ return v && v->as.str? strlen(v->as.str): 0; }
