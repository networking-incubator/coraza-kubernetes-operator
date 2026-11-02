(module
  ;; Envoy releases before 1.40 (Istio 1.31 ships Envoy 1.39) do not provide
  ;; the WASI Preview 1 functions Rust std imports, so the VM refuses to load
  ;; envoy-coraza. These ENOSYS stubs satisfy the loader only: a validated
  ;; configuration never reaches a filesystem-backed code path.
  (func (export "fd_filestat_get") (param i32 i32) (result i32) (i32.const 52))
  (func (export "fd_readdir") (param i32 i32 i32 i64 i32) (result i32) (i32.const 52))
  (func (export "path_create_directory") (param i32 i32 i32) (result i32) (i32.const 52))
  (func (export "path_filestat_get") (param i32 i32 i32 i32 i32) (result i32) (i32.const 52))
  (func (export "path_unlink_file") (param i32 i32 i32) (result i32) (i32.const 52))
)
