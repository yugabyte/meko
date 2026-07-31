/// Selects which `ConcurrencyLimiter` semaphore an operation acquires
/// (`read` vs `write`). The S3-era bucket-request metrics that used to
/// hang off this enum were removed when the YB-only store became the
/// sole backend; the variant set is preserved as-is so `YbStore` call
/// sites don't need to change shape.
#[derive(Clone, Copy, Debug)]
pub(crate) enum RequestKind {
    Get,
    Put,
    Delete,
    List,
    Copy,
    Head,
}
