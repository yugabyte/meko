use utils::tracing_span_assert::check_fields_present;

mod extractors {
    use utils::tracing_span_assert::ConstExtractor;

    pub(super) const BRANCH_ID: ConstExtractor = ConstExtractor::new("branch_id");
}

#[track_caller]
pub(crate) fn debug_assert_current_span_has_branch_id() {
    if cfg!(debug_assertions) {
        if let Err(missing) = check_fields_present!([&extractors::BRANCH_ID]) {
            panic!("missing extractors: {missing:?}")
        }
    }
}
