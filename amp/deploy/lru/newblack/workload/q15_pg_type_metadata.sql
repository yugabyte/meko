SELECT ns.nspname, t.oid, t.typname, t.typtype, t.typnotnull, t.elemtypoid
FROM (
  SELECT typ.oid, typ.typnamespace, typ.typname, typ.typtype, typ.typrelid, typ.typnotnull, typ.relkind,
         elemtyp.oid AS elemtypoid, elemtyp.typname AS elemtypname, elemcls.relkind AS elemrelkind,
         CASE WHEN elemproc.proname=$1 THEN $2 ELSE elemtyp.typtype::text END AS elemtyptype,
         typ.typcategory
  FROM (
    SELECT typ.oid, typnamespace, typname, typrelid, typnotnull, relkind, typelem AS elemoid,
           CASE WHEN proc.proname=$3 THEN $4 ELSE typ.typtype::text END AS typtype,
           CASE
             WHEN proc.proname=$5 THEN typ.typelem
             WHEN typ.typtype=$6 THEN rngsubtype
             WHEN typ.typtype=$7 THEN (SELECT rngtypid FROM pg_range WHERE rngmultitypid = typ.oid)
             WHEN typ.typtype=$8 THEN typ.typbasetype
           END AS elemtypoid,
           typ.typcategory
    FROM pg_type AS typ
    LEFT JOIN pg_class AS cls ON (cls.oid = typ.typrelid)
    LEFT JOIN pg_proc AS proc ON proc.oid = typ.typreceive
    LEFT JOIN pg_range ON (pg_range.rngtypid = typ.oid)
  ) AS typ
  LEFT JOIN pg_type AS elemtyp ON elemtyp.oid = elemtypoid
  LEFT JOIN pg_class AS elemcls ON (elemcls.oid = elemtyp.typrelid)
  LEFT JOIN pg_proc AS elemproc ON elemproc.oid = elemtyp.typreceive
) AS t
JOIN pg_namespace AS ns ON (ns.oid = typnamespace)
WHERE (
  typtype IN ($9, $10, $11, $12, $13)
  OR (typtype = $14 AND relkind::text=$15)
  OR (typtype = $16 AND typname IN ($17, $18, $19))
  OR (typtype = $20 AND (
    elemtyptype IN ($21, $22, $23, $24, $25)
    OR (elemtyptype = $26 AND elemtypname IN ($27, $28))
    OR (elemtyptype = $29 AND elemrelkind::text=$30)
  ))
)
ORDER BY CASE
  WHEN typtype IN ($31, $32, $33) THEN $34
  WHEN typtype = $35 THEN $36
  WHEN typtype = $37 THEN $38
  WHEN typtype = $39 THEN $40
  WHEN typtype = $41 AND elemtyptype <> $42 THEN $43
  WHEN typtype = $44 THEN $45
  WHEN typtype = $46 AND elemtyptype = $47 THEN $48
END;
