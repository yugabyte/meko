from __future__ import annotations

import json
import re
import threading
import time
from concurrent.futures import ThreadPoolExecutor
from datetime import datetime, timezone
from typing import Any

import psycopg
import psycopg.conninfo
from psycopg.rows import dict_row

INSPECTION_APPLICATION_NAME = "amp-time-live-explain"
PREPARED_STATEMENT_NAME = "amp_time_live_statement"
ALLOWED_EXPLAIN_KEYWORDS = {
    "DELETE",
    "INSERT",
    "MERGE",
    "SELECT",
    "TABLE",
    "UPDATE",
    "VALUES",
    "WITH",
}
SYSTEM_SCHEMAS = {"information_schema", "pg_catalog"}


class InspectionError(RuntimeError):
    """A safe, user-displayable live-inspection error."""


def _utc_iso() -> str:
    return datetime.now(timezone.utc).isoformat().replace("+00:00", "Z")


def quote_identifier(value: str) -> str:
    return '"' + str(value).replace('"', '""') + '"'


def qualified_name(schema: str, name: str) -> str:
    return f"{quote_identifier(schema)}.{quote_identifier(name)}"


def quote_literal(value: str) -> str:
    return "'" + value.replace("'", "''") + "'"


def _without_leading_comments(query: str) -> str:
    value = query.lstrip()
    while True:
        if value.startswith("/*"):
            end = value.find("*/", 2)
            if end < 0:
                return value
            value = value[end + 2 :].lstrip()
            continue
        if value.startswith("--"):
            end = value.find("\n", 2)
            if end < 0:
                return value
            value = value[end + 1 :].lstrip()
            continue
        return value


def normalize_explainable_query(query: Any) -> str:
    value = "" if query is None else str(query).strip()
    while value.endswith(";"):
        value = value[:-1].rstrip()
    if not value:
        raise InspectionError("The selected PGSS entry has no query text to explain.")
    if ";" in value:
        raise InspectionError("Multi-statement SQL is not eligible for live EXPLAIN.")
    body = _without_leading_comments(value)
    keyword_match = re.match(r"([A-Za-z]+)", body)
    keyword = keyword_match.group(1).upper() if keyword_match else ""
    if keyword not in ALLOWED_EXPLAIN_KEYWORDS:
        raise InspectionError(
            f"Live EXPLAIN supports plannable SELECT/DML statements; this entry starts with {keyword or 'an unsupported token'}."
        )
    return value


def parameter_count(query: str) -> int:
    numbers = [int(value) for value in re.findall(r"\$([1-9][0-9]*)", query)]
    return max(numbers, default=0)


def extract_plan_relations(plan: Any) -> list[dict[str, str]]:
    """Return distinct user-relation names from a PostgreSQL JSON EXPLAIN tree."""
    found: list[dict[str, str]] = []
    seen: set[tuple[str, str]] = set()

    def visit(value: Any) -> None:
        if isinstance(value, dict):
            relation = value.get("Relation Name")
            schema = value.get("Schema")
            if relation and schema and str(schema) not in SYSTEM_SCHEMAS:
                key = (str(schema), str(relation))
                if key not in seen:
                    seen.add(key)
                    found.append({"schema": key[0], "name": key[1]})
            for child in value.values():
                visit(child)
        elif isinstance(value, list):
            for child in value:
                visit(child)

    visit(plan)
    return found


def render_create_table(
    *,
    schema: str,
    name: str,
    columns: list[dict[str, Any]],
    constraints: list[dict[str, Any]],
    indexes: list[str],
    relkind: str = "r",
    view_definition: str | None = None,
    is_partition: bool = False,
    partition_parent_schema: str | None = None,
    partition_parent_name: str | None = None,
    partition_bound: str | None = None,
    partition_key: str | None = None,
    owner: str | None = None,
    relation_comment: str | None = None,
    grants: list[dict[str, Any]] | None = None,
    policies: list[dict[str, Any]] | None = None,
    triggers: list[str] | None = None,
    replica_identity: str = "d",
    replica_identity_index: str | None = None,
    reloptions: list[str] | None = None,
    tablespace: str | None = None,
    row_security: bool = False,
    force_row_security: bool = False,
    persistence: str = "p",
) -> tuple[str, str]:
    qualified = qualified_name(schema, name)
    grants = grants or []
    policies = policies or []
    triggers = triggers or []
    reloptions = reloptions or []
    object_type = {"v": "VIEW", "m": "MATERIALIZED VIEW"}.get(relkind, "TABLE")
    if relkind in {"v", "m"}:
        prefix = "CREATE OR REPLACE VIEW" if relkind == "v" else "CREATE MATERIALIZED VIEW"
        create_table = f"{prefix} {qualified} AS\n{(view_definition or '').rstrip()};"
    elif is_partition and partition_parent_schema and partition_parent_name and partition_bound:
        parent = qualified_name(partition_parent_schema, partition_parent_name)
        create_table = f"CREATE TABLE {qualified} PARTITION OF {parent} {partition_bound}"
        if reloptions:
            create_table += f" WITH ({', '.join(reloptions)})"
        if tablespace:
            create_table += f" TABLESPACE {quote_identifier(tablespace)}"
        create_table += ";"
    else:
        definitions: list[str] = []
        for column in columns:
            line = f"{quote_identifier(str(column['name']))} {column['data_type']}"
            if column.get("collation_schema") and column.get("collation_name"):
                line += " COLLATE " + qualified_name(
                    str(column["collation_schema"]), str(column["collation_name"])
                )
            identity = str(column.get("identity") or "")
            generated = str(column.get("generated") or "")
            default_expr = column.get("default_expr")
            if identity in {"a", "d"}:
                mode = "ALWAYS" if identity == "a" else "BY DEFAULT"
                line += f" GENERATED {mode} AS IDENTITY"
                sequence = column.get("identity_sequence")
                if isinstance(sequence, dict) and sequence.get("name"):
                    options = [
                        f"SEQUENCE NAME {qualified_name(str(sequence['schema']), str(sequence['name']))}",
                        f"START WITH {sequence['start']}",
                        f"INCREMENT BY {sequence['increment']}",
                        f"MINVALUE {sequence['minimum']}",
                        f"MAXVALUE {sequence['maximum']}",
                        f"CACHE {sequence['cache']}",
                        "CYCLE" if sequence.get("cycle") else "NO CYCLE",
                    ]
                    line += f" ({' '.join(options)})"
            elif generated:
                storage = "VIRTUAL" if generated == "v" else "STORED"
                line += f" GENERATED ALWAYS AS ({default_expr}) {storage}"
            elif default_expr is not None:
                line += f" DEFAULT {default_expr}"
            if column.get("not_null"):
                line += " NOT NULL"
            definitions.append(line)

        for constraint in constraints:
            definitions.append(
                f"CONSTRAINT {quote_identifier(str(constraint['name']))} {constraint['definition']}"
            )

        indented = ",\n".join(f"    {line}" for line in definitions)
        prefix = "CREATE UNLOGGED TABLE" if persistence == "u" else "CREATE TABLE"
        create_table = f"{prefix} {qualified} (\n{indented}\n)"
        if relkind == "p" and partition_key:
            create_table += f" PARTITION BY {partition_key}"
        if reloptions:
            create_table += f" WITH ({', '.join(reloptions)})"
        if tablespace:
            create_table += f" TABLESPACE {quote_identifier(tablespace)}"
        create_table += ";"

    sections = [create_table]
    if owner:
        sections.append(f"ALTER {object_type} {qualified} OWNER TO {quote_identifier(owner)};")
    if relation_comment is not None:
        sections.append(f"COMMENT ON {object_type} {qualified} IS {quote_literal(relation_comment)};")
    for column in columns:
        if column.get("comment") is not None:
            sections.append(
                f"COMMENT ON COLUMN {qualified}.{quote_identifier(str(column['name']))} "
                f"IS {quote_literal(str(column['comment']))};"
            )
    sections.extend(f"{value.rstrip(';')};" for value in indexes)
    if relkind in {"r", "p"} and replica_identity == "n":
        sections.append(f"ALTER TABLE {qualified} REPLICA IDENTITY NOTHING;")
    elif relkind in {"r", "p"} and replica_identity == "f":
        sections.append(f"ALTER TABLE {qualified} REPLICA IDENTITY FULL;")
    elif relkind in {"r", "p"} and replica_identity == "i" and replica_identity_index:
        sections.append(
            f"ALTER TABLE {qualified} REPLICA IDENTITY USING INDEX "
            f"{quote_identifier(replica_identity_index)};"
        )
    if relkind in {"r", "p"} and row_security:
        sections.append(f"ALTER TABLE {qualified} ENABLE ROW LEVEL SECURITY;")
    if relkind in {"r", "p"} and force_row_security:
        sections.append(f"ALTER TABLE {qualified} FORCE ROW LEVEL SECURITY;")
    policy_commands = {"*": "ALL", "r": "SELECT", "a": "INSERT", "w": "UPDATE", "d": "DELETE"}
    for policy in policies:
        roles = policy.get("roles") or ["PUBLIC"]
        role_sql = ", ".join(
            "PUBLIC" if str(role) == "PUBLIC" else quote_identifier(str(role)) for role in roles
        )
        policy_sql = (
            f"CREATE POLICY {quote_identifier(str(policy['name']))} ON {qualified} "
            f"AS {'PERMISSIVE' if policy.get('permissive', True) else 'RESTRICTIVE'} "
            f"FOR {policy_commands.get(str(policy.get('command') or '*'), 'ALL')} TO {role_sql}"
        )
        if policy.get("using") is not None:
            policy_sql += f" USING ({policy['using']})"
        if policy.get("with_check") is not None:
            policy_sql += f" WITH CHECK ({policy['with_check']})"
        sections.append(policy_sql + ";")
    sections.extend(f"{value.rstrip(';')};" for value in triggers)
    grouped_grants: dict[tuple[str, bool], list[str]] = {}
    for grant in grants:
        key = (str(grant.get("grantee") or "PUBLIC"), bool(grant.get("grantable")))
        grouped_grants.setdefault(key, []).append(str(grant["privilege"]).upper())
    for (grantee, grantable), privileges in grouped_grants.items():
        grantee_sql = "PUBLIC" if grantee == "PUBLIC" else quote_identifier(grantee)
        grant_sql = (
            f"GRANT {', '.join(privileges)} ON TABLE {qualified} TO {grantee_sql}"
        )
        if grantable:
            grant_sql += " WITH GRANT OPTION"
        sections.append(grant_sql + ";")
    combined = "\n\n".join(sections)
    return create_table, combined


def _first_value(row: dict[str, Any] | None) -> Any:
    if not row:
        return None
    return next(iter(row.values()))


def _safe_number(value: Any) -> float | int | None:
    if value is None:
        return None
    try:
        number = float(value)
    except (TypeError, ValueError):
        return None
    return int(number) if number.is_integer() else number


class LiveInspector:
    """Safely plan one stored PGSS query and reconstruct its referenced relation DDL."""

    def __init__(self, dsn: str, password: str | None = None, max_concurrent: int = 2) -> None:
        self.dsn = dsn
        self.password = password
        self._slots = threading.BoundedSemaphore(max_concurrent)

    def _target_dsn(self, dbname: str) -> str:
        info = psycopg.conninfo.conninfo_to_dict(self.dsn)
        info["dbname"] = dbname
        if self.password:
            info["password"] = self.password
        return psycopg.conninfo.make_conninfo(**info)

    def _redact(self, message: str) -> str:
        secrets = [self.password]
        try:
            secrets.append(psycopg.conninfo.conninfo_to_dict(self.dsn).get("password"))
        except Exception:
            pass
        for secret in secrets:
            if secret:
                message = message.replace(secret, "********")
        return message

    def inspect(self, statement: dict[str, Any]) -> dict[str, Any]:
        if not self._slots.acquire(blocking=False):
            raise InspectionError("Two live inspections are already running; try again in a moment.")
        try:
            return self._inspect(statement)
        finally:
            self._slots.release()

    def _inspect(self, statement: dict[str, Any]) -> dict[str, Any]:
        query = normalize_explainable_query(statement.get("query"))
        dbname = str(statement.get("dbname") or "").strip()
        if not dbname or dbname == "[dropped database]":
            raise InspectionError("The statement database is no longer available for live planning.")
        started = time.perf_counter()
        try:
            # TEXT and JSON are independent EXPLAIN formats. Separate read-only
            # sessions let PostgreSQL connection setup and planning overlap.
            with ThreadPoolExecutor(max_workers=2, thread_name_prefix="amp-time-explain") as pool:
                text_future = pool.submit(self._explain_text, dbname, query)
                catalog_future = pool.submit(self._explain_json_and_catalog, dbname, query)
                plan_text, text_params = text_future.result()
                catalog = catalog_future.result()
            params = int(catalog["parameter_count"])
            if params != text_params:
                raise InspectionError("PostgreSQL inferred inconsistent parameter counts.")
            search_path = str(catalog["search_path"])
            relations = list(catalog["relations"])
            ddl = list(catalog["ddl"])
        except InspectionError:
            raise
        except psycopg.Error as exc:
            detail = exc.diag.message_primary if exc.diag else str(exc)
            raise InspectionError(self._redact(f"PostgreSQL could not produce a live plan: {detail}")) from exc

        elapsed_ms = round((time.perf_counter() - started) * 1000, 2)
        ddl_text = "\n\n".join(str(item["sql"]) for item in ddl)
        if not ddl_text:
            ddl_text = "-- no user relations"
        statistics = [
            {
                "schema": item["schema"],
                "name": item["name"],
                "qualified_name": item["qualified_name"],
                "reltuples": item["reltuples"],
                "n_live_tup": item["n_live_tup"],
                "n_dead_tup": item["n_dead_tup"],
                "n_mod_since_analyze": item["n_mod_since_analyze"],
                "last_analyze": item["last_analyze"],
                "last_autoanalyze": item["last_autoanalyze"],
                "columns": item["column_statistics"],
            }
            for item in ddl
        ]
        return {
            "mode": "GENERIC LIVE PLAN",
            "analyze": False,
            "captured_at_utc": _utc_iso(),
            "elapsed_ms": elapsed_ms,
            "database": dbname,
            "planned_as": psycopg.conninfo.conninfo_to_dict(self.dsn).get("user", ""),
            "recorded_user": statement.get("username") or "",
            "queryid": str(statement.get("queryid") or ""),
            "query": query,
            "search_path": search_path,
            "parameter_count": params,
            "parameter_strategy": (
                f"plan_cache_mode=force_generic_plan · parameters={params} · values=NULL"
                if params
                else "plan_cache_mode=force_generic_plan · parameters=0"
            ),
            "plan_text": plan_text,
            "pgss_lifetime": {
                "calls": _safe_number(statement.get("pgss_calls")),
                "total_ms": _safe_number(statement.get("pgss_total_ms")),
                "min_ms": _safe_number(statement.get("pgss_min_ms")),
                "max_ms": _safe_number(statement.get("pgss_max_ms")),
                "mean_ms": _safe_number(statement.get("pgss_mean_ms")),
                "stddev_ms": _safe_number(statement.get("pgss_stddev_ms")),
                "stats_since": statement.get("stats_since"),
                "minmax_stats_since": statement.get("minmax_stats_since"),
            },
            "relations": relations,
            "ddl": ddl,
            "ddl_text": ddl_text,
            "statistics": statistics,
        }

    def _connect(self, dbname: str, suffix: str) -> psycopg.Connection[dict[str, Any]]:
        return psycopg.connect(
            self._target_dsn(dbname),
            autocommit=True,
            row_factory=dict_row,
            application_name=f"{INSPECTION_APPLICATION_NAME}-{suffix}",
            connect_timeout=10,
            options=(
                "-c default_transaction_read_only=on "
                "-c statement_timeout=5000 "
                "-c lock_timeout=1500 "
                "-c idle_in_transaction_session_timeout=8000 "
                "-c plan_cache_mode=force_generic_plan"
            ),
        )

    def _prepare(self, cur: psycopg.Cursor[dict[str, Any]], query: str) -> tuple[int, str]:
        # The selected text comes from a server-side snapshot, not the request body.
        # Semicolons are rejected before this point, so it cannot escape PREPARE.
        cur.execute(f"PREPARE {PREPARED_STATEMENT_NAME} AS {query}")
        cur.execute(
            """/* amp-time:explain */
            WITH amp_time_prepared AS (
                SELECT COALESCE(array_length(parameter_types, 1), 0)::integer AS parameter_count
                FROM pg_prepared_statements
                WHERE name = %s
            )
            SELECT parameter_count FROM amp_time_prepared
            """,
            (PREPARED_STATEMENT_NAME,),
        )
        prepared = cur.fetchone()
        if not prepared:
            raise InspectionError("PostgreSQL did not register the prepared statement.")
        params = int(prepared["parameter_count"])
        execute_sql = f"EXECUTE {PREPARED_STATEMENT_NAME}"
        if params:
            execute_sql += f"({', '.join('NULL' for _ in range(params))})"
        return params, execute_sql

    def _explain_text(self, dbname: str, query: str) -> tuple[str, int]:
        with self._connect(dbname, "text") as conn, conn.cursor() as cur:
            params, execute_sql = self._prepare(cur, query)
            cur.execute(
                "EXPLAIN (ANALYZE FALSE, VERBOSE TRUE, COSTS TRUE, SETTINGS TRUE, "
                f"SUMMARY FALSE, FORMAT TEXT) {execute_sql}"
            )
            plan_text = "\n".join(str(_first_value(row) or "") for row in cur.fetchall())
            return plan_text, params

    def _explain_json_and_catalog(self, dbname: str, query: str) -> dict[str, Any]:
        with self._connect(dbname, "catalog") as conn, conn.cursor() as cur:
            cur.execute(
                "WITH amp_time_settings AS (SELECT current_setting('search_path') AS value) "
                "SELECT value FROM amp_time_settings"
            )
            search_path = str(_first_value(cur.fetchone()) or "")
            params, execute_sql = self._prepare(cur, query)
            cur.execute(
                "EXPLAIN (ANALYZE FALSE, VERBOSE TRUE, COSTS TRUE, SETTINGS TRUE, "
                f"SUMMARY FALSE, FORMAT JSON) {execute_sql}"
            )
            plan_json = _first_value(cur.fetchone())
            if isinstance(plan_json, str):
                plan_json = json.loads(plan_json)
            relations = extract_plan_relations(plan_json)
            cur.execute(
                "WITH amp_time_set_search_path AS ("
                "SELECT set_config('search_path', '', false) AS value"
                ") SELECT value FROM amp_time_set_search_path"
            )
            ddl = self._relations_ddl(cur, relations)
            return {
                "parameter_count": params,
                "search_path": search_path,
                "relations": relations,
                "ddl": ddl,
            }

    @staticmethod
    def _json_list(value: Any) -> list[Any]:
        if isinstance(value, str):
            value = json.loads(value)
        return value if isinstance(value, list) else []

    def _relations_ddl(
        self, cur: psycopg.Cursor[dict[str, Any]], relations: list[dict[str, str]]
    ) -> list[dict[str, Any]]:
        if not relations:
            return []
        cur.execute(
            """/* amp-time:catalog */
            WITH amp_time_requested AS (
                SELECT
                    item->>'schema' AS schema_name,
                    item->>'name' AS relation_name,
                    ordinality
                FROM jsonb_array_elements(%s::jsonb) WITH ORDINALITY AS requested(item, ordinality)
            ),
            amp_time_relations AS (
                SELECT
                    requested.ordinality,
                    n.nspname::text AS schema_name,
                    c.relname::text AS relation_name,
                    c.oid,
                    c.relkind::text AS relkind,
                    c.reltuples::float8 AS reltuples,
                    c.relpersistence::text AS persistence,
                    c.relreplident::text AS replica_identity,
                    c.relrowsecurity AS row_security,
                    c.relforcerowsecurity AS force_row_security,
                    c.reloptions,
                    c.relacl,
                    pg_get_userbyid(c.relowner)::text AS owner_name,
                    obj_description(c.oid, 'pg_class') AS relation_comment,
                    ts.spcname::text AS tablespace_name,
                    replica_index.relname::text AS replica_identity_index,
                    table_stats.n_live_tup::bigint AS n_live_tup,
                    table_stats.n_dead_tup::bigint AS n_dead_tup,
                    table_stats.n_mod_since_analyze::bigint AS n_mod_since_analyze,
                    table_stats.last_analyze,
                    table_stats.last_autoanalyze,
                    c.relispartition,
                    pg_get_partkeydef(c.oid) AS partition_key,
                    pn.nspname::text AS partition_parent_schema,
                    pc.relname::text AS partition_parent_name,
                    pg_get_expr(c.relpartbound, c.oid, true) AS partition_bound,
                    CASE WHEN c.relkind IN ('v', 'm') THEN pg_get_viewdef(c.oid, true) END AS view_definition
                FROM amp_time_requested requested
                JOIN pg_namespace n ON n.nspname = requested.schema_name
                JOIN pg_class c ON c.relnamespace = n.oid AND c.relname = requested.relation_name
                LEFT JOIN pg_inherits i ON i.inhrelid = c.oid AND c.relispartition
                LEFT JOIN pg_class pc ON pc.oid = i.inhparent
                LEFT JOIN pg_namespace pn ON pn.oid = pc.relnamespace
                LEFT JOIN pg_tablespace ts ON ts.oid = c.reltablespace
                LEFT JOIN pg_stat_all_tables table_stats ON table_stats.relid = c.oid
                LEFT JOIN pg_index replica_identity ON
                    replica_identity.indrelid = c.oid AND replica_identity.indisreplident
                LEFT JOIN pg_class replica_index ON replica_index.oid = replica_identity.indexrelid
                WHERE c.relkind IN ('r', 'p', 'v', 'm')
            )
            SELECT
                relation.ordinality,
                relation.schema_name,
                relation.relation_name,
                relation.relkind,
                relation.reltuples,
                relation.persistence,
                relation.replica_identity,
                relation.row_security,
                relation.force_row_security,
                relation.reloptions,
                relation.owner_name,
                relation.relation_comment,
                relation.tablespace_name,
                relation.replica_identity_index,
                relation.n_live_tup,
                relation.n_dead_tup,
                relation.n_mod_since_analyze,
                relation.last_analyze,
                relation.last_autoanalyze,
                relation.relispartition,
                relation.partition_key,
                relation.partition_parent_schema,
                relation.partition_parent_name,
                relation.partition_bound,
                relation.view_definition,
                COALESCE((
                    SELECT jsonb_agg(
                        jsonb_build_object(
                            'name', column_data.name,
                            'data_type', column_data.data_type,
                            'not_null', column_data.not_null,
                            'identity', column_data.identity,
                            'generated', column_data.generated,
                            'default_expr', column_data.default_expr,
                            'comment', column_data.comment,
                            'identity_sequence', column_data.identity_sequence,
                            'collation_schema', column_data.collation_schema,
                            'collation_name', column_data.collation_name
                        ) ORDER BY column_data.attnum
                    )
                    FROM (
                        SELECT
                            a.attnum,
                            a.attname::text AS name,
                            format_type(a.atttypid, a.atttypmod) AS data_type,
                            a.attnotnull AS not_null,
                            a.attidentity::text AS identity,
                            a.attgenerated::text AS generated,
                            pg_get_expr(d.adbin, d.adrelid, true) AS default_expr,
                            col_description(a.attrelid, a.attnum) AS comment,
                            CASE WHEN a.attidentity <> '' THEN (
                                SELECT jsonb_build_object(
                                    'schema', sequence_namespace.nspname::text,
                                    'name', sequence_class.relname::text,
                                    'start', sequence_data.seqstart,
                                    'increment', sequence_data.seqincrement,
                                    'minimum', sequence_data.seqmin,
                                    'maximum', sequence_data.seqmax,
                                    'cache', sequence_data.seqcache,
                                    'cycle', sequence_data.seqcycle
                                )
                                FROM pg_depend sequence_dependency
                                JOIN pg_class sequence_class
                                  ON sequence_class.oid = sequence_dependency.objid
                                 AND sequence_class.relkind = 'S'
                                JOIN pg_namespace sequence_namespace
                                  ON sequence_namespace.oid = sequence_class.relnamespace
                                JOIN pg_sequence sequence_data
                                  ON sequence_data.seqrelid = sequence_class.oid
                                WHERE sequence_dependency.refobjid = a.attrelid
                                  AND sequence_dependency.refobjsubid = a.attnum
                                  AND sequence_dependency.classid = 'pg_class'::regclass
                                  AND sequence_dependency.refclassid = 'pg_class'::regclass
                                  AND sequence_dependency.deptype IN ('i', 'a')
                                LIMIT 1
                            ) END AS identity_sequence,
                            CASE WHEN a.attcollation <> 0 AND a.attcollation <> t.typcollation
                                 THEN cn.nspname::text END AS collation_schema,
                            CASE WHEN a.attcollation <> 0 AND a.attcollation <> t.typcollation
                                 THEN co.collname::text END AS collation_name
                        FROM pg_attribute a
                        JOIN pg_type t ON t.oid = a.atttypid
                        LEFT JOIN pg_attrdef d ON d.adrelid = a.attrelid AND d.adnum = a.attnum
                        LEFT JOIN pg_collation co ON co.oid = a.attcollation
                        LEFT JOIN pg_namespace cn ON cn.oid = co.collnamespace
                        WHERE a.attrelid = relation.oid AND a.attnum > 0 AND NOT a.attisdropped
                    ) column_data
                ), '[]'::jsonb) AS columns,
                COALESCE((
                    SELECT jsonb_agg(
                        jsonb_build_object(
                            'name', constraint_data.name,
                            'type', constraint_data.type,
                            'definition', constraint_data.definition
                        ) ORDER BY constraint_data.sort_order, constraint_data.name
                    )
                    FROM (
                        SELECT
                            conname::text AS name,
                            contype::text AS type,
                            pg_get_constraintdef(oid, true) AS definition,
                            CASE contype
                                WHEN 'p' THEN 1 WHEN 'u' THEN 2 WHEN 'c' THEN 3
                                WHEN 'x' THEN 4 WHEN 'f' THEN 5 ELSE 6
                            END AS sort_order
                        FROM pg_constraint
                        WHERE conrelid = relation.oid
                          AND contype IN ('p', 'u', 'c', 'x', 'f')
                    ) constraint_data
                ), '[]'::jsonb) AS constraints,
                COALESCE((
                    SELECT jsonb_agg(index_data.indexdef ORDER BY index_data.relname)
                    FROM (
                        SELECT idx.relname::text, pg_get_indexdef(i.indexrelid, 0, true) AS indexdef
                        FROM pg_index i
                        JOIN pg_class idx ON idx.oid = i.indexrelid
                        LEFT JOIN pg_constraint con ON con.conindid = i.indexrelid
                        WHERE i.indrelid = relation.oid AND con.oid IS NULL
                    ) index_data
                ), '[]'::jsonb) AS indexes,
                COALESCE((
                    SELECT jsonb_agg(
                        jsonb_build_object(
                            'grantee', CASE WHEN acl.grantee = 0 THEN 'PUBLIC'
                                            ELSE pg_get_userbyid(acl.grantee) END,
                            'privilege', acl.privilege_type,
                            'grantable', acl.is_grantable
                        ) ORDER BY acl.grantee, acl.privilege_type
                    )
                    FROM aclexplode(relation.relacl) acl
                ), '[]'::jsonb) AS grants,
                COALESCE((
                    SELECT jsonb_agg(
                        jsonb_build_object(
                            'name', policy.polname,
                            'permissive', policy.polpermissive,
                            'command', policy.polcmd,
                            'roles', (
                                SELECT jsonb_agg(
                                    CASE WHEN role_oid = 0 THEN 'PUBLIC'
                                         ELSE pg_get_userbyid(role_oid) END ORDER BY role_oid
                                )
                                FROM unnest(policy.polroles) role_oid
                            ),
                            'using', pg_get_expr(policy.polqual, policy.polrelid, true),
                            'with_check', pg_get_expr(policy.polwithcheck, policy.polrelid, true)
                        ) ORDER BY policy.polname
                    )
                    FROM pg_policy policy
                    WHERE policy.polrelid = relation.oid
                ), '[]'::jsonb) AS policies,
                COALESCE((
                    SELECT jsonb_agg(pg_get_triggerdef(trigger.oid, true) ORDER BY trigger.tgname)
                    FROM pg_trigger trigger
                    WHERE trigger.tgrelid = relation.oid AND NOT trigger.tgisinternal
                ), '[]'::jsonb) AS triggers,
                COALESCE((
                    SELECT jsonb_agg(
                        jsonb_build_object(
                            'attname', stats_data.attname,
                            'null_frac', stats_data.null_frac,
                            'avg_width', stats_data.avg_width,
                            'n_distinct', stats_data.n_distinct
                        ) ORDER BY stats_data.attnum
                    )
                    FROM (
                        SELECT DISTINCT ON (stats.attname)
                            attribute.attnum,
                            stats.attname::text AS attname,
                            stats.null_frac::float8 AS null_frac,
                            stats.avg_width::integer AS avg_width,
                            stats.n_distinct::float8 AS n_distinct,
                            stats.inherited
                        FROM pg_stats stats
                        JOIN pg_attribute attribute
                          ON attribute.attrelid = relation.oid AND attribute.attname = stats.attname
                        WHERE stats.schemaname = relation.schema_name
                          AND stats.tablename = relation.relation_name
                        ORDER BY stats.attname, stats.inherited
                    ) stats_data
                ), '[]'::jsonb) AS column_statistics
            FROM amp_time_relations relation
            ORDER BY relation.ordinality
            """,
            (json.dumps(relations),),
        )

        result: list[dict[str, Any]] = []
        for relation in cur.fetchall():
            schema = str(relation["schema_name"])
            name = str(relation["relation_name"])
            relkind = str(relation["relkind"])
            columns = self._json_list(relation["columns"])
            constraints = self._json_list(relation["constraints"])
            indexes = [str(value) for value in self._json_list(relation["indexes"])]
            grants = self._json_list(relation["grants"])
            policies = self._json_list(relation["policies"])
            triggers = [str(value) for value in self._json_list(relation["triggers"])]
            column_statistics = self._json_list(relation["column_statistics"])
            reloptions = [str(value) for value in (relation["reloptions"] or [])]
            create_table, sql = render_create_table(
                schema=schema,
                name=name,
                columns=columns,
                constraints=constraints,
                indexes=indexes,
                relkind=relkind,
                view_definition=relation["view_definition"],
                is_partition=bool(relation["relispartition"]),
                partition_parent_schema=relation["partition_parent_schema"],
                partition_parent_name=relation["partition_parent_name"],
                partition_bound=relation["partition_bound"],
                partition_key=relation["partition_key"],
                owner=relation["owner_name"],
                relation_comment=relation["relation_comment"],
                grants=grants,
                policies=policies,
                triggers=triggers,
                replica_identity=str(relation["replica_identity"] or "d"),
                replica_identity_index=relation["replica_identity_index"],
                reloptions=reloptions,
                tablespace=relation["tablespace_name"],
                row_security=bool(relation["row_security"]),
                force_row_security=bool(relation["force_row_security"]),
                persistence=str(relation["persistence"] or "p"),
            )
            result.append(
                {
                    "schema": schema,
                    "name": name,
                    "qualified_name": f"{schema}.{name}",
                    "relation_kind": relkind,
                    "reltuples": float(relation["reltuples"]),
                    "n_live_tup": relation["n_live_tup"],
                    "n_dead_tup": relation["n_dead_tup"],
                    "n_mod_since_analyze": relation["n_mod_since_analyze"],
                    "last_analyze": relation["last_analyze"].isoformat()
                    if relation["last_analyze"] is not None
                    else None,
                    "last_autoanalyze": relation["last_autoanalyze"].isoformat()
                    if relation["last_autoanalyze"] is not None
                    else None,
                    "owner": relation["owner_name"],
                    "tablespace": relation["tablespace_name"],
                    "replica_identity": relation["replica_identity"],
                    "storage_parameters": reloptions,
                    "column_statistics": column_statistics,
                    "create_table": create_table,
                    "indexes": indexes,
                    "grants": grants,
                    "policies": policies,
                    "triggers": triggers,
                    "sql": sql,
                }
            )
        return result
