# Summary

Date : 2026-05-31 10:24:35

Directory /net/dev-server-hsunder/share/code/yb-light/yb-pageserver

Total : 149 files,  66226 codes, 14320 comments, 9282 blanks, all 89828 lines

Summary / [Details](details.md) / [Diff Summary](diff.md) / [Diff Details](diff-details.md)

## Languages
| language | files | code | comment | blank | total |
| :--- | ---: | ---: | ---: | ---: | ---: |
| Rust | 142 | 64,697 | 14,031 | 9,161 | 87,889 |
| YAML | 1 | 1,360 | 0 | 66 | 1,426 |
| Protocol Buffers | 2 | 114 | 289 | 35 | 438 |
| Markdown | 3 | 54 | 0 | 19 | 73 |
| JSON | 1 | 1 | 0 | 1 | 2 |

## Directories
| path | files | code | comment | blank | total |
| :--- | ---: | ---: | ---: | ---: | ---: |
| . | 149 | 66,226 | 14,320 | 9,282 | 89,828 |
| benches | 6 | 1,295 | 398 | 147 | 1,840 |
| client | 4 | 1,094 | 45 | 129 | 1,268 |
| client/src | 4 | 1,094 | 45 | 129 | 1,268 |
| client/src (Files) | 3 | 1,049 | 41 | 124 | 1,214 |
| client/src/mgmt_api | 1 | 45 | 4 | 5 | 54 |
| client_grpc | 4 | 877 | 398 | 138 | 1,413 |
| client_grpc/src | 4 | 877 | 398 | 138 | 1,413 |
| compaction | 10 | 2,314 | 471 | 395 | 3,180 |
| compaction (Files) | 1 | 42 | 0 | 10 | 52 |
| compaction/src | 8 | 2,220 | 463 | 374 | 3,057 |
| compaction/src (Files) | 6 | 1,716 | 424 | 289 | 2,429 |
| compaction/src/bin | 1 | 167 | 14 | 36 | 217 |
| compaction/src/simulator | 1 | 337 | 25 | 49 | 411 |
| compaction/tests | 1 | 52 | 8 | 11 | 71 |
| ctl | 8 | 1,598 | 193 | 213 | 2,004 |
| ctl/src | 8 | 1,598 | 193 | 213 | 2,004 |
| page_api | 7 | 1,084 | 457 | 189 | 1,730 |
| page_api (Files) | 1 | 10 | 2 | 2 | 14 |
| page_api/proto | 2 | 114 | 289 | 35 | 438 |
| page_api/proto (Files) | 1 | 101 | 160 | 29 | 290 |
| page_api/proto/google | 1 | 13 | 129 | 6 | 148 |
| page_api/proto/google/protobuf | 1 | 13 | 129 | 6 | 148 |
| page_api/src | 4 | 960 | 166 | 152 | 1,278 |
| pagebench | 10 | 1,727 | 107 | 218 | 2,052 |
| pagebench/src | 10 | 1,727 | 107 | 218 | 2,052 |
| pagebench/src (Files) | 1 | 72 | 8 | 13 | 93 |
| pagebench/src/cmd | 6 | 1,519 | 94 | 181 | 1,794 |
| pagebench/src/util | 3 | 136 | 5 | 24 | 165 |
| pagebench/src/util (Files) | 2 | 108 | 5 | 17 | 130 |
| pagebench/src/util/cli | 1 | 28 | 0 | 7 | 35 |
| src | 98 | 56,232 | 12,251 | 7,848 | 76,331 |
| src (Files) | 27 | 24,922 | 5,235 | 3,153 | 33,310 |
| src/bin | 2 | 781 | 143 | 124 | 1,048 |
| src/config | 1 | 148 | 12 | 20 | 180 |
| src/deletion_queue | 3 | 728 | 211 | 128 | 1,067 |
| src/http | 3 | 4,058 | 218 | 468 | 4,744 |
| src/tenant | 42 | 22,667 | 5,846 | 3,531 | 32,044 |
| src/tenant (Files) | 17 | 12,054 | 3,442 | 1,877 | 17,373 |
| src/tenant/layer_map | 2 | 593 | 165 | 84 | 842 |
| src/tenant/remote_timeline_client | 4 | 1,054 | 302 | 178 | 1,534 |
| src/tenant/storage_layer | 8 | 4,807 | 991 | 801 | 6,599 |
| src/tenant/storage_layer (Files) | 7 | 4,460 | 943 | 773 | 6,176 |
| src/tenant/storage_layer/inmemory_layer | 1 | 347 | 48 | 28 | 423 |
| src/tenant/timeline | 11 | 4,159 | 946 | 591 | 5,696 |
| src/tenant/timeline (Files) | 8 | 2,724 | 694 | 411 | 3,829 |
| src/tenant/timeline/import_pgdata | 1 | 68 | 0 | 8 | 76 |
| src/tenant/timeline/walreceiver | 2 | 1,367 | 252 | 172 | 1,791 |
| src/virtual_file | 16 | 2,291 | 429 | 342 | 3,062 |
| src/virtual_file (Files) | 4 | 660 | 66 | 62 | 788 |
| src/virtual_file/io_engine | 1 | 165 | 17 | 19 | 201 |
| src/virtual_file/owned_buffers_io | 11 | 1,466 | 346 | 261 | 2,073 |
| src/virtual_file/owned_buffers_io (Files) | 5 | 569 | 144 | 94 | 807 |
| src/virtual_file/owned_buffers_io/aligned_buffer | 5 | 552 | 142 | 122 | 816 |
| src/virtual_file/owned_buffers_io/write | 1 | 345 | 60 | 45 | 450 |
| src/walredo | 4 | 637 | 157 | 82 | 876 |
| src/walredo (Files) | 2 | 509 | 132 | 55 | 696 |
| src/walredo/process | 2 | 128 | 25 | 27 | 180 |
| test_data | 2 | 5 | 0 | 5 | 10 |
| test_data/indices | 2 | 5 | 0 | 5 | 10 |
| test_data/indices/mixed_workload | 2 | 5 | 0 | 5 | 10 |

Summary / [Details](details.md) / [Diff Summary](diff.md) / [Diff Details](diff-details.md)