#include "vector_kernels.h"
#include <math.h>

static inline float dot8(const float *a, const float *b) {
    float s0 = 0, s1 = 0, s2 = 0, s3 = 0;
    float s4 = 0, s5 = 0, s6 = 0, s7 = 0;
    s0 += a[0] * b[0];
    s1 += a[1] * b[1];
    s2 += a[2] * b[2];
    s3 += a[3] * b[3];
    s4 += a[4] * b[4];
    s5 += a[5] * b[5];
    s6 += a[6] * b[6];
    s7 += a[7] * b[7];
    return s0 + s1 + s2 + s3 + s4 + s5 + s6 + s7;
}

float dot32(const float *a, const float *b, size_t n) {
    float s0 = 0, s1 = 0, s2 = 0, s3 = 0;
    float s4 = 0, s5 = 0, s6 = 0, s7 = 0;
    size_t i = 0;
    size_t n8 = n & ~((size_t)7);
    for (; i < n8; i += 8) {
        s0 += a[i+0] * b[i+0];
        s1 += a[i+1] * b[i+1];
        s2 += a[i+2] * b[i+2];
        s3 += a[i+3] * b[i+3];
        s4 += a[i+4] * b[i+4];
        s5 += a[i+5] * b[i+5];
        s6 += a[i+6] * b[i+6];
        s7 += a[i+7] * b[i+7];
    }
    for (; i < n; i++) {
        s0 += a[i] * b[i];
    }
    return s0 + s1 + s2 + s3 + s4 + s5 + s6 + s7;
}

float squared_l2(const float *a, const float *b, size_t n) {
    float s0 = 0, s1 = 0, s2 = 0, s3 = 0;
    float s4 = 0, s5 = 0, s6 = 0, s7 = 0;
    size_t i = 0;
    size_t n8 = n & ~((size_t)7);
    for (; i < n8; i += 8) {
        float d0 = a[i+0] - b[i+0];
        float d1 = a[i+1] - b[i+1];
        float d2 = a[i+2] - b[i+2];
        float d3 = a[i+3] - b[i+3];
        float d4 = a[i+4] - b[i+4];
        float d5 = a[i+5] - b[i+5];
        float d6 = a[i+6] - b[i+6];
        float d7 = a[i+7] - b[i+7];
        s0 += d0 * d0;
        s1 += d1 * d1;
        s2 += d2 * d2;
        s3 += d3 * d3;
        s4 += d4 * d4;
        s5 += d5 * d5;
        s6 += d6 * d6;
        s7 += d7 * d7;
    }
    for (; i < n; i++) {
        float d = a[i] - b[i];
        s0 += d * d;
    }
    return s0 + s1 + s2 + s3 + s4 + s5 + s6 + s7;
}

float norm32(const float *a, size_t n) {
    float d = dot32(a, a, n);
    // simple sqrt via stdlib
    return sqrtf(d);
}

int normalize32(float *a, size_t n) {
    float nrm = norm32(a, n);
    if (nrm == 0.0f) return 0;
    float inv = 1.0f / nrm;
    for (size_t i = 0; i < n; i++) {
        a[i] *= inv;
    }
    return 1;
}
