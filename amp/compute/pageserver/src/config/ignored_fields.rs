//! Check for fields in the on-disk config file that were ignored when
//! deserializing [`pageserver_api::config::ConfigToml`].
//!
//! This could have been part of the [`pageserver_api::config`] module,
//! but the way we identify unused fields in this module
//! is specific to the format (TOML) and the implementation of the
//! deserialization for that format ([`toml_edit`]).

use std::collections::HashSet;

use itertools::Itertools;

/// Pass in the user-specified config and the re-serialized [`pageserver_api::config::ConfigToml`].
/// The returned [`Paths`] contains the paths to the fields that were ignored by deserialization
/// of the [`pageserver_api::config::ConfigToml`].
pub fn find(user_specified: toml_edit::DocumentMut, reserialized: toml_edit::DocumentMut) -> Paths {
    let user_specified = paths(user_specified);
    let reserialized = paths(reserialized);
    fn paths(doc: toml_edit::DocumentMut) -> HashSet<String> {
        let mut out = Vec::new();
        let mut visitor = PathsVisitor::new(&mut out);
        visitor.visit_table_like(doc.as_table());
        HashSet::from_iter(out)
    }

    let mut ignored = HashSet::new();

    // O(n) because of HashSet
    for path in user_specified {
        if !reserialized.contains(&path) {
            ignored.insert(path);
        }
    }

    Paths {
        paths: ignored
            .into_iter()
            // sort lexicographically for deterministic output
            .sorted()
            .collect(),
    }
}

pub struct Paths {
    pub paths: Vec<String>,
}

struct PathsVisitor<'a> {
    stack: Vec<String>,
    out: &'a mut Vec<String>,
}

impl<'a> PathsVisitor<'a> {
    fn new(out: &'a mut Vec<String>) -> Self {
        Self {
            stack: Vec::new(),
            out,
        }
    }

    fn visit_table_like(&mut self, table_like: &dyn toml_edit::TableLike) {
        for (entry, item) in table_like.iter() {
            self.stack.push(entry.to_string());
            self.visit_item(item);
            self.stack.pop();
        }
    }

    fn visit_item(&mut self, item: &toml_edit::Item) {
        match item {
            toml_edit::Item::None => (),
            toml_edit::Item::Value(value) => self.visit_value(value),
            toml_edit::Item::Table(table) => {
                self.visit_table_like(table);
            }
            toml_edit::Item::ArrayOfTables(array_of_tables) => {
                for (i, table) in array_of_tables.iter().enumerate() {
                    self.stack.push(format!("[{i}]"));
                    self.visit_table_like(table);
                    self.stack.pop();
                }
            }
        }
    }

    fn visit_value(&mut self, value: &toml_edit::Value) {
        match value {
            toml_edit::Value::String(_)
            | toml_edit::Value::Integer(_)
            | toml_edit::Value::Float(_)
            | toml_edit::Value::Boolean(_)
            | toml_edit::Value::Datetime(_) => self.out.push(self.stack.join(".")),
            toml_edit::Value::Array(array) => {
                for (i, value) in array.iter().enumerate() {
                    self.stack.push(format!("[{i}]"));
                    self.visit_value(value);
                    self.stack.pop();
                }
            }
            toml_edit::Value::InlineTable(inline_table) => self.visit_table_like(inline_table),
        }
    }
}

#[cfg(test)]
pub(crate) mod tests {

    fn test_impl(original: &str, parsed: &str, expect: [&str; 1]) {
        let original: toml_edit::DocumentMut = original.parse().expect("parse original config");
        let parsed: toml_edit::DocumentMut = parsed.parse().expect("parse re-serialized config");

        let super::Paths { paths: actual } = super::find(original, parsed);
        assert_eq!(actual, &expect);
    }

    #[test]
    fn top_level() {
        test_impl(
            r#"
                [a]
                b = 1
                c = 2
                d = 3
            "#,
            r#"
                [a]
                b = 1
                c = 2
            "#,
            ["a.d"],
        );
    }

    #[test]
    fn nested() {
        test_impl(
            r#"
                [a.b.c]
                d = 23
            "#,
            r#"
                [a]
                e = 42
            "#,
            ["a.b.c.d"],
        );
    }

    #[test]
    fn array_of_tables() {
        test_impl(
            r#"
                [[a]]
                b = 1
                c = 2
                d = 3
            "#,
            r#"
                [[a]]
                b = 1
                c = 2
            "#,
            ["a.[0].d"],
        );
    }

    #[test]
    fn array() {
        test_impl(
            r#"
            foo = [ {bar = 23} ]
            "#,
            r#"
            foo = [ { blup = 42 }]
            "#,
            ["foo.[0].bar"],
        );
    }

    /// `test_impl` for any number of expected paths, including none.
    fn expect_paths(original: &str, parsed: &str, expect: &[&str]) {
        let original: toml_edit::DocumentMut = original.parse().expect("parse original config");
        let parsed: toml_edit::DocumentMut = parsed.parse().expect("parse re-serialized config");

        let super::Paths { paths: actual } = super::find(original, parsed);
        assert_eq!(actual, expect);
    }

    /// The endpoint supervisor writes overrides as dotted keys, while serde
    /// re-serializes in the section form, so the two spellings meet here and must
    /// compare equal. If they stop doing so, every dotted override becomes an
    /// "unknown configuration item" and a working endpoint turns into a crash loop.
    #[test]
    fn dotted_key_matches_the_section_form() {
        expect_paths(
            r#"
                scale_factor = 4.0
                tenant_config.enable_gc = true
                tenant_config.gc_horizon = 1024
            "#,
            r#"
                scale_factor = 4.0

                [tenant_config]
                enable_gc = true
                gc_horizon = 1024
            "#,
            &[],
        );
    }

    /// The other half: dotted spelling must not blind the check. A genuinely
    /// unknown nested key is still reported, so a typo in an override is caught
    /// rather than silently ignored.
    #[test]
    fn an_unknown_dotted_key_is_still_reported() {
        expect_paths(
            r#"
                tenant_config.enable_gc = true
                tenant_config.enable_gc_typo = true
            "#,
            r#"
                [tenant_config]
                enable_gc = true
            "#,
            &["tenant_config.enable_gc_typo"],
        );
    }

    /// A known blind spot, pinned so nobody assumes otherwise: the stack is joined
    /// with ".", so a quoted key containing a dot is indistinguishable from a
    /// nested one. Serde drops such a key and this check cannot see it, so the
    /// setting goes missing with no error anywhere — which is why the supervisor
    /// emits real nested tables rather than relying on this check.
    #[test]
    fn a_literal_dotted_key_is_indistinguishable_from_a_nested_one() {
        expect_paths(
            r#"
                "tenant_config.enable_gc" = true
            "#,
            r#"
                [tenant_config]
                enable_gc = true
            "#,
            &[],
        );
    }
}
