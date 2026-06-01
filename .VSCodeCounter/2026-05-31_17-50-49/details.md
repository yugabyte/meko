# Details

Date : 2026-05-31 17:50:49

Directory /net/dev-server-hsunder/share/code/yb-light/yb-pageserver/src

Total : 74 files,  40866 codes, 8796 comments, 5476 blanks, all 55138 lines

[Summary](results.md) / Details / [Diff Summary](diff.md) / [Diff Details](diff-details.md)

## Files
| filename | language | code | comment | blank | total |
| :--- | :--- | ---: | ---: | ---: | ---: |
| [yb-pageserver/src/assert\_u64\_eq\_usize.rs](/yb-pageserver/src/assert_u64_eq_usize.rs) | Rust | 23 | 1 | 5 | 29 |
| [yb-pageserver/src/auth.rs](/yb-pageserver/src/auth.rs) | Rust | 33 | 0 | 2 | 35 |
| [yb-pageserver/src/aux\_file.rs](/yb-pageserver/src/aux_file.rs) | Rust | 231 | 33 | 27 | 291 |
| [yb-pageserver/src/basebackup.rs](/yb-pageserver/src/basebackup.rs) | Rust | 670 | 121 | 76 | 867 |
| [yb-pageserver/src/basebackup\_cache.rs](/yb-pageserver/src/basebackup_cache.rs) | Rust | 510 | 99 | 95 | 704 |
| [yb-pageserver/src/bin/test\_helper\_slow\_client\_reads.rs](/yb-pageserver/src/bin/test_helper_slow_client_reads.rs) | Rust | 52 | 0 | 10 | 62 |
| [yb-pageserver/src/bin/yb-pageserver.rs](/yb-pageserver/src/bin/yb-pageserver.rs) | Rust | 723 | 143 | 112 | 978 |
| [yb-pageserver/src/config.rs](/yb-pageserver/src/config.rs) | Rust | 597 | 164 | 110 | 871 |
| [yb-pageserver/src/config/ignored\_fields.rs](/yb-pageserver/src/config/ignored_fields.rs) | Rust | 148 | 12 | 20 | 180 |
| [yb-pageserver/src/context.rs](/yb-pageserver/src/context.rs) | Rust | 298 | 173 | 52 | 523 |
| [yb-pageserver/src/controller\_upcall\_client.rs](/yb-pageserver/src/controller_upcall_client.rs) | Rust | 289 | 26 | 38 | 353 |
| [yb-pageserver/src/deletion\_queue.rs](/yb-pageserver/src/deletion_queue.rs) | Rust | 192 | 58 | 30 | 280 |
| [yb-pageserver/src/http/mod.rs](/yb-pageserver/src/http/mod.rs) | Rust | 2 | 0 | 1 | 3 |
| [yb-pageserver/src/http/openapi\_spec.yml](/yb-pageserver/src/http/openapi_spec.yml) | YAML | 1,360 | 0 | 66 | 1,426 |
| [yb-pageserver/src/http/routes.rs](/yb-pageserver/src/http/routes.rs) | Rust | 2,496 | 202 | 368 | 3,066 |
| [yb-pageserver/src/image\_materializer.rs](/yb-pageserver/src/image_materializer.rs) | Rust | 274 | 118 | 25 | 417 |
| [yb-pageserver/src/import\_datadir.rs](/yb-pageserver/src/import_datadir.rs) | Rust | 506 | 66 | 85 | 657 |
| [yb-pageserver/src/l0\_flush.rs](/yb-pageserver/src/l0_flush.rs) | Rust | 40 | 1 | 8 | 49 |
| [yb-pageserver/src/lib.rs](/yb-pageserver/src/lib.rs) | Rust | 287 | 58 | 47 | 392 |
| [yb-pageserver/src/metrics.rs](/yb-pageserver/src/metrics.rs) | Rust | 3,303 | 288 | 381 | 3,972 |
| [yb-pageserver/src/page\_service.rs](/yb-pageserver/src/page_service.rs) | Rust | 3,681 | 568 | 385 | 4,634 |
| [yb-pageserver/src/page\_store.rs](/yb-pageserver/src/page_store.rs) | Rust | 1,618 | 262 | 122 | 2,002 |
| [yb-pageserver/src/page\_writer.rs](/yb-pageserver/src/page_writer.rs) | Rust | 1,379 | 357 | 95 | 1,831 |
| [yb-pageserver/src/pgdatadir\_mapping.rs](/yb-pageserver/src/pgdatadir_mapping.rs) | Rust | 2,358 | 481 | 323 | 3,162 |
| [yb-pageserver/src/span.rs](/yb-pageserver/src/span.rs) | Rust | 38 | 0 | 6 | 44 |
| [yb-pageserver/src/task\_mgr.rs](/yb-pageserver/src/task_mgr.rs) | Rust | 365 | 216 | 76 | 657 |
| [yb-pageserver/src/tenant.rs](/yb-pageserver/src/tenant.rs) | Rust | 3,582 | 831 | 519 | 4,932 |
| [yb-pageserver/src/tenant/config.rs](/yb-pageserver/src/tenant/config.rs) | Rust | 166 | 71 | 24 | 261 |
| [yb-pageserver/src/tenant/gc\_block.rs](/yb-pageserver/src/tenant/gc_block.rs) | Rust | 149 | 39 | 37 | 225 |
| [yb-pageserver/src/tenant/gc\_result.rs](/yb-pageserver/src/tenant/gc_result.rs) | Rust | 34 | 4 | 7 | 45 |
| [yb-pageserver/src/tenant/metadata.rs](/yb-pageserver/src/tenant/metadata.rs) | Rust | 284 | 87 | 63 | 434 |
| [yb-pageserver/src/tenant/mgr.rs](/yb-pageserver/src/tenant/mgr.rs) | Rust | 1,816 | 466 | 264 | 2,546 |
| [yb-pageserver/src/tenant/remote\_timeline\_client.rs](/yb-pageserver/src/tenant/remote_timeline_client.rs) | Rust | 1,256 | 502 | 197 | 1,955 |
| [yb-pageserver/src/tenant/remote\_timeline\_client/download.rs](/yb-pageserver/src/tenant/remote_timeline_client/download.rs) | Rust | 402 | 63 | 54 | 519 |
| [yb-pageserver/src/tenant/remote\_timeline\_client/index.rs](/yb-pageserver/src/tenant/remote_timeline_client/index.rs) | Rust | 199 | 129 | 52 | 380 |
| [yb-pageserver/src/tenant/remote\_timeline\_client/manifest.rs](/yb-pageserver/src/tenant/remote_timeline_client/manifest.rs) | Rust | 40 | 37 | 12 | 89 |
| [yb-pageserver/src/tenant/remote\_timeline\_client/upload.rs](/yb-pageserver/src/tenant/remote_timeline_client/upload.rs) | Rust | 148 | 18 | 22 | 188 |
| [yb-pageserver/src/tenant/storage\_layer.rs](/yb-pageserver/src/tenant/storage_layer.rs) | Rust | 79 | 18 | 16 | 113 |
| [yb-pageserver/src/tenant/storage\_layer/layer\_name.rs](/yb-pageserver/src/tenant/storage_layer/layer_name.rs) | Rust | 297 | 43 | 48 | 388 |
| [yb-pageserver/src/tenant/tasks.rs](/yb-pageserver/src/tenant/tasks.rs) | Rust | 328 | 70 | 44 | 442 |
| [yb-pageserver/src/tenant/throttle.rs](/yb-pageserver/src/tenant/throttle.rs) | Rust | 92 | 26 | 18 | 136 |
| [yb-pageserver/src/tenant/timeline.rs](/yb-pageserver/src/tenant/timeline.rs) | Rust | 2,434 | 720 | 409 | 3,563 |
| [yb-pageserver/src/tenant/timeline/delete.rs](/yb-pageserver/src/tenant/timeline/delete.rs) | Rust | 371 | 106 | 62 | 539 |
| [yb-pageserver/src/tenant/timeline/handle.rs](/yb-pageserver/src/tenant/timeline/handle.rs) | Rust | 339 | 263 | 35 | 637 |
| [yb-pageserver/src/tenant/timeline/import\_pgdata.rs](/yb-pageserver/src/tenant/timeline/import_pgdata.rs) | Rust | 23 | 7 | 9 | 39 |
| [yb-pageserver/src/tenant/timeline/import\_pgdata/index\_part\_format.rs](/yb-pageserver/src/tenant/timeline/import_pgdata/index_part_format.rs) | Rust | 68 | 0 | 8 | 76 |
| [yb-pageserver/src/tenant/timeline/logical\_size.rs](/yb-pageserver/src/tenant/timeline/logical_size.rs) | Rust | 113 | 54 | 22 | 189 |
| [yb-pageserver/src/tenant/timeline/uninit.rs](/yb-pageserver/src/tenant/timeline/uninit.rs) | Rust | 254 | 51 | 38 | 343 |
| [yb-pageserver/src/tenant/timeline/walreceiver.rs](/yb-pageserver/src/tenant/timeline/walreceiver.rs) | Rust | 179 | 50 | 22 | 251 |
| [yb-pageserver/src/tenant/timeline/walreceiver/connection\_manager.rs](/yb-pageserver/src/tenant/timeline/walreceiver/connection_manager.rs) | Rust | 879 | 154 | 98 | 1,131 |
| [yb-pageserver/src/tenant/timeline/walreceiver/walreceiver\_connection.rs](/yb-pageserver/src/tenant/timeline/walreceiver/walreceiver_connection.rs) | Rust | 488 | 98 | 74 | 660 |
| [yb-pageserver/src/tenant/upload\_queue.rs](/yb-pageserver/src/tenant/upload_queue.rs) | Rust | 324 | 111 | 58 | 493 |
| [yb-pageserver/src/unified\_pages.rs](/yb-pageserver/src/unified_pages.rs) | Rust | 430 | 119 | 45 | 594 |
| [yb-pageserver/src/utilization.rs](/yb-pageserver/src/utilization.rs) | Rust | 49 | 10 | 15 | 74 |
| [yb-pageserver/src/virtual\_file.rs](/yb-pageserver/src/virtual_file.rs) | Rust | 823 | 424 | 133 | 1,380 |
| [yb-pageserver/src/virtual\_file/io\_engine.rs](/yb-pageserver/src/virtual_file/io_engine.rs) | Rust | 394 | 38 | 26 | 458 |
| [yb-pageserver/src/virtual\_file/io\_engine/tokio\_epoll\_uring\_ext.rs](/yb-pageserver/src/virtual_file/io_engine/tokio_epoll_uring_ext.rs) | Rust | 165 | 17 | 19 | 201 |
| [yb-pageserver/src/virtual\_file/metadata.rs](/yb-pageserver/src/virtual_file/metadata.rs) | Rust | 26 | 0 | 5 | 31 |
| [yb-pageserver/src/virtual\_file/open\_options.rs](/yb-pageserver/src/virtual_file/open_options.rs) | Rust | 160 | 10 | 18 | 188 |
| [yb-pageserver/src/virtual\_file/owned\_buffers\_io/aligned\_buffer.rs](/yb-pageserver/src/virtual_file/owned_buffers_io/aligned_buffer.rs) | Rust | 8 | 0 | 2 | 10 |
| [yb-pageserver/src/virtual\_file/owned\_buffers\_io/aligned\_buffer/alignment.rs](/yb-pageserver/src/virtual_file/owned_buffers_io/aligned_buffer/alignment.rs) | Rust | 19 | 3 | 5 | 27 |
| [yb-pageserver/src/virtual\_file/owned\_buffers\_io/aligned\_buffer/buffer.rs](/yb-pageserver/src/virtual_file/owned_buffers_io/aligned_buffer/buffer.rs) | Rust | 97 | 14 | 23 | 134 |
| [yb-pageserver/src/virtual\_file/owned\_buffers\_io/aligned\_buffer/buffer\_mut.rs](/yb-pageserver/src/virtual_file/owned_buffers_io/aligned_buffer/buffer_mut.rs) | Rust | 279 | 64 | 56 | 399 |
| [yb-pageserver/src/virtual\_file/owned\_buffers\_io/aligned\_buffer/raw.rs](/yb-pageserver/src/virtual_file/owned_buffers_io/aligned_buffer/raw.rs) | Rust | 128 | 57 | 30 | 215 |
| [yb-pageserver/src/virtual\_file/owned\_buffers\_io/aligned\_buffer/slice.rs](/yb-pageserver/src/virtual_file/owned_buffers_io/aligned_buffer/slice.rs) | Rust | 29 | 4 | 8 | 41 |
| [yb-pageserver/src/virtual\_file/owned\_buffers\_io/io\_buf\_aligned.rs](/yb-pageserver/src/virtual_file/owned_buffers_io/io_buf_aligned.rs) | Rust | 6 | 2 | 6 | 14 |
| [yb-pageserver/src/virtual\_file/owned\_buffers\_io/io\_buf\_ext.rs](/yb-pageserver/src/virtual_file/owned_buffers_io/io_buf_ext.rs) | Rust | 75 | 14 | 13 | 102 |
| [yb-pageserver/src/virtual\_file/owned\_buffers\_io/slice.rs](/yb-pageserver/src/virtual_file/owned_buffers_io/slice.rs) | Rust | 88 | 18 | 14 | 120 |
| [yb-pageserver/src/walingest.rs](/yb-pageserver/src/walingest.rs) | Rust | 1,252 | 219 | 151 | 1,622 |
| [yb-pageserver/src/walredo.rs](/yb-pageserver/src/walredo.rs) | Rust | 384 | 161 | 48 | 593 |
| [yb-pageserver/src/walredo/apply\_neon.rs](/yb-pageserver/src/walredo/apply_neon.rs) | Rust | 225 | 37 | 20 | 282 |
| [yb-pageserver/src/walredo/process.rs](/yb-pageserver/src/walredo/process.rs) | Rust | 284 | 95 | 35 | 414 |
| [yb-pageserver/src/walredo/process/no\_leak\_child.rs](/yb-pageserver/src/walredo/process/no_leak_child.rs) | Rust | 84 | 19 | 14 | 117 |
| [yb-pageserver/src/walredo/process/protocol.rs](/yb-pageserver/src/walredo/process/protocol.rs) | Rust | 44 | 6 | 13 | 63 |

[Summary](results.md) / Details / [Diff Summary](diff.md) / [Diff Details](diff-details.md)