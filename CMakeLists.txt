cmake_minimum_required(VERSION 3.16)
project(graingraph_c C)

set(CMAKE_C_STANDARD 11)

add_library(fastjson STATIC c/fastjson.c)
target_include_directories(fastjson PUBLIC c)
