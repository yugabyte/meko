SELECT typ.oid, att.attname, att.atttypid
FROM pg_type AS typ
JOIN pg_namespace AS ns ON (ns.oid = typ.typnamespace)
JOIN pg_class AS cls ON (cls.oid = typ.typrelid)
JOIN pg_attribute AS att ON (att.attrelid = typ.typrelid)
WHERE (typ.typtype::text = $1 AND cls.relkind::text=$2)
  AND attnum > $3
  AND NOT attisdropped
ORDER BY typ.oid, att.attnum;
