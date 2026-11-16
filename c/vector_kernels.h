#pragma once
#include <stddef.h>
#include <stdint.h>

#ifdef __cplusplus
extern "C" {
#endif

float dot32(const float *a, const float *b, size_t n);
float squared_l2(const float *a, const float *b, size_t n);
float norm32(const float *a, size_t n);
int normalize32(float *a, size_t n);

#ifdef __cplusplus
}
#endif
