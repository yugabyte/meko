-- Cutdown commerce schema for benchmark-touched objects only.
-- Generated from bomber_schema.sql by selecting workload-touched tables (+region _1 partitions), required composite types, and indexes.

CREATE TYPE public.salesledgertoinsert AS (
    id uuid,
    orderid uuid,
    orderlineid uuid,
    typeid uuid,
    creationtime timestamp with time zone,
    description text,
    oldvalue text,
    newvalue text,
    regionid smallint
);

CREATE TYPE public.itemledgertoinsert AS (
    id uuid,
    typeid integer,
    productid uuid,
    organizationunitid uuid,
    quantity integer,
    quantityallocated integer,
    remark text,
    stocklabelid uuid,
    issummary boolean,
    stockmutationid uuid,
    orderlineid uuid,
    createdbyid uuid,
    creationtime timestamp with time zone
);

CREATE FUNCTION public.empty_uuid() RETURNS uuid
    LANGUAGE sql IMMUTABLE PARALLEL SAFE
    AS $$select uuid '00000000-0000-0000-0000-000000000000'$$;

CREATE FUNCTION public.get_enum_tables_sql(table_names text[]) RETURNS text
    LANGUAGE plpgsql
    AS $$
    declare
        n text;
        sql text;
        tables text[];
    begin
        tables = array(
            select * from (
                select t.table_schema || '.' || t.table_name as table_name
                from information_schema.tables t
            ) x
            where x.table_name = any(table_names)
        );

        sql := '';

        foreach n in array table_names loop
            if exists (
                select *
                from unnest(tables) as t(name)
                where t.name = n
            ) then
                sql := sql || format('select * from %s;', n);
            else
                sql := sql || 'select 1 where 1 = 0;';
            end if;
        end loop;

        return sql;
    end;
$$;

CREATE FUNCTION public.get_system_id() RETURNS uuid
    LANGUAGE sql IMMUTABLE PARALLEL SAFE
    AS $$select uuid '00000000-0000-0000-ffff-ffffffffffff'$$;


SET default_tablespace = region_1;


--
-- Name: locations; Type: TABLE; Schema: public; Owner: admin
--

CREATE TABLE public.locations (
    id uuid NOT NULL,
    regionid smallint NOT NULL,
    addressedto character varying(150),
    zipcode character varying(20),
    housenumber character varying(100),
    street character varying(100),
    city character varying(100),
    countryid character varying(3) NOT NULL,
    createdbyid uuid NOT NULL,
    creationtime timestamp with time zone NOT NULL,
    lastmodifiedbyid uuid,
    lastmodificationtime timestamp with time zone,
    latitude double precision,
    longitude double precision,
    region character varying(50),
    address1 character varying(100),
    address2 character varying(100),
    district character varying(50),
    subdistrict character varying(50),
    state character varying(64),
    firstname character varying(150),
    phonenumber character varying(30),
    emailaddress character varying(100),
    countrysubdivisionid character varying(10),
    remark character varying(1024)
)
PARTITION BY LIST (regionid);


--
-- Name: locations_1; Type: TABLE; Schema: public; Owner: admin; Tablespace: region_1
--

CREATE TABLE public.locations_1 (
    id uuid NOT NULL,
    regionid smallint NOT NULL,
    addressedto character varying(150),
    zipcode character varying(20),
    housenumber character varying(100),
    street character varying(100),
    city character varying(100),
    countryid character varying(3) NOT NULL,
    createdbyid uuid NOT NULL,
    creationtime timestamp with time zone NOT NULL,
    lastmodifiedbyid uuid,
    lastmodificationtime timestamp with time zone,
    latitude double precision,
    longitude double precision,
    region character varying(50),
    address1 character varying(100),
    address2 character varying(100),
    district character varying(50),
    subdistrict character varying(50),
    state character varying(64),
    firstname character varying(150),
    phonenumber character varying(30),
    emailaddress character varying(100),
    countrysubdivisionid character varying(10),
    remark character varying(1024),
    CONSTRAINT addresses_1_pkey PRIMARY KEY((id) HASH, regionid ASC)
)
SPLIT INTO 6 TABLETS;


--
-- Name: accesskeys; Type: TABLE; Schema: public; Owner: admin
--

CREATE TABLE public.accesskeys (
    id uuid NOT NULL,
    userid uuid NOT NULL,
    authenticationtokenhash character varying(100) NOT NULL,
    isactive boolean NOT NULL,
    expirationdate timestamp with time zone,
    createdbyid uuid NOT NULL,
    creationtime timestamp with time zone NOT NULL,
    lastmodifiedbyid uuid,
    lastmodificationtime timestamp with time zone,
    organizationunitid uuid,
    languageid character varying(2),
    temporaryaccesstype character varying(255),
    temporaryaccessdata character varying(255),
    description character varying(512),
    islockedtoorganizationunit boolean DEFAULT false NOT NULL,
    lastaccesstime timestamp with time zone,
    isstrict boolean DEFAULT false NOT NULL,
    CONSTRAINT apikeys_pkey PRIMARY KEY(id ASC)
);


--
-- Name: eventchainledger_1; Type: TABLE; Schema: public; Owner: admin; Tablespace: region_1
--

CREATE TABLE public.eventchainledger_1 (
    id uuid NOT NULL,
    chainid uuid NOT NULL,
    newsequencenumber bigint NOT NULL,
    previoussequencenumber bigint,
    newsignature character varying(2048),
    previoussignature character varying(2048),
    createdbyid uuid NOT NULL,
    creationtime timestamp with time zone NOT NULL,
    regionid smallint NOT NULL,
    CONSTRAINT auditchainledger_1_pkey PRIMARY KEY((id) HASH, regionid ASC)
)
SPLIT INTO 6 TABLETS;


--
-- Name: eventchainlocks_1; Type: TABLE; Schema: public; Owner: admin; Tablespace: region_1
--

CREATE TABLE public.eventchainlocks_1 (
    id integer NOT NULL,
    typeid integer NOT NULL,
    regionid smallint NOT NULL,
    CONSTRAINT auditchainlocks_1_pkey PRIMARY KEY((id) HASH, typeid ASC, regionid ASC)
)
SPLIT INTO 6 TABLETS;


--
-- Name: eventchains_1; Type: TABLE; Schema: public; Owner: admin; Tablespace: region_1
--

CREATE TABLE public.eventchains_1 (
    id uuid NOT NULL,
    regionid smallint NOT NULL,
    identifier character varying(255) NOT NULL,
    discriminator bigint NOT NULL,
    typeid integer NOT NULL,
    version integer NOT NULL,
    lastmodifiedbyid uuid,
    lastmodificationtime timestamp with time zone,
    createdbyid uuid NOT NULL,
    creationtime timestamp with time zone NOT NULL,
    organizationunitid uuid DEFAULT public.empty_uuid() NOT NULL,
    isoffline boolean DEFAULT false NOT NULL,
    state jsonb NOT NULL,
    CONSTRAINT auditchains_1_pkey PRIMARY KEY((id) HASH, regionid ASC)
)
SPLIT INTO 6 TABLETS;


--
-- Name: businesses; Type: TABLE; Schema: public; Owner: admin
--

CREATE TABLE public.businesses (
    id uuid NOT NULL,
    regionid smallint NOT NULL,
    createdbyid uuid NOT NULL,
    creationtime timestamp with time zone NOT NULL,
    lastmodifiedbyid uuid,
    lastmodificationtime timestamp with time zone,
    name character varying(255) NOT NULL,
    registrationnumber character varying(128),
    vatnumber character varying(128),
    addressid uuid,
    invoiceaddressid uuid,
    invoiceemailaddress character varying(255),
    contactemailaddress character varying(255),
    accountholdername character varying(255),
    iban character varying(34),
    bic character varying(11),
    logoid uuid,
    contactphonenumber character varying(30),
    registrationcity character varying(255),
    registrationcountryid character varying(3),
    establisheddate date,
    fiscalid character varying(20),
    wastedisposalnumber character varying(16),
    taxofficenumber character varying(32),
    auditingfields character varying,
    registrationcountrysubdivisionid character varying(10),
    typeid integer DEFAULT 0 NOT NULL,
    originatingid uuid,
    backendid character varying(255),
    socialcapital numeric(18,4),
    industrycode character varying(255),
    entitytype character varying(255),
    isdeleted boolean DEFAULT false NOT NULL,
    taxrecipientcode character varying(255),
    taxrecipientemail character varying(255),
    singleuse boolean DEFAULT false NOT NULL,
    isinvoicing boolean DEFAULT true NOT NULL,
    registrationaddressid uuid,
    customfields text,
    vatnumbervalidationresultid integer DEFAULT 0 NOT NULL,
    hasusers boolean DEFAULT false NOT NULL,
    activitydescription character varying(50),
    establishmentnumber character varying(255)
)
PARTITION BY LIST (regionid);


--
-- Name: businesses_1; Type: TABLE; Schema: public; Owner: admin; Tablespace: region_1
--

CREATE TABLE public.businesses_1 (
    id uuid NOT NULL,
    regionid smallint NOT NULL,
    createdbyid uuid NOT NULL,
    creationtime timestamp with time zone NOT NULL,
    lastmodifiedbyid uuid,
    lastmodificationtime timestamp with time zone,
    name character varying(255) NOT NULL,
    registrationnumber character varying(128),
    vatnumber character varying(128),
    addressid uuid,
    invoiceaddressid uuid,
    invoiceemailaddress character varying(255),
    contactemailaddress character varying(255),
    accountholdername character varying(255),
    iban character varying(34),
    bic character varying(11),
    logoid uuid,
    contactphonenumber character varying(30),
    registrationcity character varying(255),
    registrationcountryid character varying(3),
    establisheddate date,
    fiscalid character varying(20),
    wastedisposalnumber character varying(16),
    taxofficenumber character varying(32),
    auditingfields character varying,
    registrationcountrysubdivisionid character varying(10),
    typeid integer DEFAULT 0 NOT NULL,
    originatingid uuid,
    backendid character varying(255),
    socialcapital numeric(18,4),
    industrycode character varying(255),
    entitytype character varying(255),
    isdeleted boolean DEFAULT false NOT NULL,
    taxrecipientcode character varying(255),
    taxrecipientemail character varying(255),
    singleuse boolean DEFAULT false NOT NULL,
    isinvoicing boolean DEFAULT true NOT NULL,
    registrationaddressid uuid,
    customfields text,
    vatnumbervalidationresultid integer DEFAULT 0 NOT NULL,
    hasusers boolean DEFAULT false NOT NULL,
    activitydescription character varying(50),
    establishmentnumber character varying(255),
    CONSTRAINT companies_1_pkey PRIMARY KEY((id) HASH, regionid ASC)
)
SPLIT INTO 6 TABLETS;


--
-- Name: pricediscounts; Type: TABLE; Schema: public; Owner: admin
--

CREATE TABLE public.pricediscounts (
    id uuid NOT NULL,
    regionid smallint NOT NULL,
    parentorderlineid uuid NOT NULL,
    discountorderlineid uuid NOT NULL,
    discountid uuid NOT NULL,
    discountamount numeric(18,4) NOT NULL,
    ledgerclassid character varying(10),
    orderid uuid NOT NULL,
    compliancyruleid integer
)
PARTITION BY LIST (regionid);


--
-- Name: pricediscounts_1; Type: TABLE; Schema: public; Owner: admin; Tablespace: region_1
--

CREATE TABLE public.pricediscounts_1 (
    id uuid NOT NULL,
    regionid smallint NOT NULL,
    parentorderlineid uuid NOT NULL,
    discountorderlineid uuid NOT NULL,
    discountid uuid NOT NULL,
    discountamount numeric(18,4) NOT NULL,
    ledgerclassid character varying(10),
    orderid uuid NOT NULL,
    compliancyruleid integer,
    CONSTRAINT discountdetails_1_pkey PRIMARY KEY((orderid) HASH, regionid ASC, id ASC)
)
SPLIT INTO 6 TABLETS;


--
-- Name: accountingperiods; Type: TABLE; Schema: public; Owner: admin
--

CREATE TABLE public.accountingperiods (
    id uuid NOT NULL,
    regionid smallint NOT NULL,
    previousperiodid uuid,
    openingtime timestamp with time zone,
    closingtime timestamp with time zone,
    statusid integer NOT NULL,
    organizationunitid uuid NOT NULL,
    userid uuid,
    createdbyid uuid NOT NULL,
    creationtime timestamp with time zone NOT NULL,
    lastmodifiedbyid uuid,
    lastmodificationtime timestamp with time zone,
    currencyid character varying(3) NOT NULL,
    costpricecurrencyid character varying(3) NOT NULL,
    financialperiodauditid uuid,
    autoclosed boolean,
    financialperiodnumber character varying(50),
    chainid uuid,
    pciquestionanswer boolean,
    expectedclosingtime timestamp with time zone,
    loyaltyprogramid uuid,
    openedbyuserid uuid,
    autoopened boolean
)
PARTITION BY LIST (regionid);


--
-- Name: accountingperiods_1; Type: TABLE; Schema: public; Owner: admin; Tablespace: region_1
--

CREATE TABLE public.accountingperiods_1 (
    id uuid NOT NULL,
    regionid smallint NOT NULL,
    previousperiodid uuid,
    openingtime timestamp with time zone,
    closingtime timestamp with time zone,
    statusid integer NOT NULL,
    organizationunitid uuid NOT NULL,
    userid uuid,
    createdbyid uuid NOT NULL,
    creationtime timestamp with time zone NOT NULL,
    lastmodifiedbyid uuid,
    lastmodificationtime timestamp with time zone,
    currencyid character varying(3) NOT NULL,
    costpricecurrencyid character varying(3) NOT NULL,
    financialperiodauditid uuid,
    autoclosed boolean,
    financialperiodnumber character varying(50),
    chainid uuid,
    pciquestionanswer boolean,
    expectedclosingtime timestamp with time zone,
    loyaltyprogramid uuid,
    openedbyuserid uuid,
    autoopened boolean,
    CONSTRAINT financialperiods_1_pkey PRIMARY KEY((id) HASH, regionid ASC)
)
SPLIT INTO 6 TABLETS;


--
-- Name: billinglines; Type: TABLE; Schema: public; Owner: admin
--

CREATE TABLE public.billinglines (
    id uuid NOT NULL,
    regionid smallint NOT NULL,
    createdbyid uuid NOT NULL,
    creationtime timestamp with time zone NOT NULL,
    invoiceid uuid NOT NULL,
    orderlineid uuid NOT NULL,
    quantityinvoiced integer NOT NULL,
    invoicedate timestamp with time zone,
    discountamount numeric(19,5) NOT NULL,
    invoicedbyid uuid NOT NULL,
    unitcost numeric(18,4),
    quantitytoinvoice integer NOT NULL,
    shipmentlineid uuid,
    orderid uuid,
    amount numeric(18,4),
    taxrate numeric(7,6),
    unitprice numeric(16,4),
    totalamountinvoiced numeric(16,4) DEFAULT 0 NOT NULL,
    totalamountinvoicedintax numeric(16,4) DEFAULT 0 NOT NULL,
    expectedunitprice numeric(16,4),
    expecteddiscountamount numeric(16,4),
    expectedtaxrate numeric(7,6),
    expectedquantity integer,
    taxcodeid uuid,
    typeid integer,
    discountdata character varying,
    financialperiodid uuid,
    taxamount numeric(18,4),
    taxratecategoryid integer,
    description character varying(255),
    ecotax numeric(18,4),
    unitpriceintax numeric(18,4) DEFAULT 0 NOT NULL,
    nettaxamount numeric(18,4) DEFAULT 0 NOT NULL,
    totaltaxamount numeric(18,4) DEFAULT 0 NOT NULL
)
PARTITION BY LIST (regionid);


--
-- Name: billinglines_1; Type: TABLE; Schema: public; Owner: admin; Tablespace: region_1
--

CREATE TABLE public.billinglines_1 (
    id uuid NOT NULL,
    regionid smallint NOT NULL,
    createdbyid uuid NOT NULL,
    creationtime timestamp with time zone NOT NULL,
    invoiceid uuid NOT NULL,
    orderlineid uuid NOT NULL,
    quantityinvoiced integer NOT NULL,
    invoicedate timestamp with time zone,
    discountamount numeric(19,5) NOT NULL,
    invoicedbyid uuid NOT NULL,
    unitcost numeric(18,4),
    quantitytoinvoice integer NOT NULL,
    shipmentlineid uuid,
    orderid uuid,
    amount numeric(18,4),
    taxrate numeric(7,6),
    unitprice numeric(16,4),
    totalamountinvoiced numeric(16,4) DEFAULT 0 NOT NULL,
    totalamountinvoicedintax numeric(16,4) DEFAULT 0 NOT NULL,
    expectedunitprice numeric(16,4),
    expecteddiscountamount numeric(16,4),
    expectedtaxrate numeric(7,6),
    expectedquantity integer,
    taxcodeid uuid,
    typeid integer,
    discountdata character varying,
    financialperiodid uuid,
    taxamount numeric(18,4),
    taxratecategoryid integer,
    description character varying(255),
    ecotax numeric(18,4),
    unitpriceintax numeric(18,4) DEFAULT 0 NOT NULL,
    nettaxamount numeric(18,4) DEFAULT 0 NOT NULL,
    totaltaxamount numeric(18,4) DEFAULT 0 NOT NULL,
    CONSTRAINT invoicelines_1_pkey PRIMARY KEY((invoiceid) HASH, regionid ASC, id ASC)
)
SPLIT INTO 6 TABLETS;


--
-- Name: billingmanualdata; Type: TABLE; Schema: public; Owner: admin
--

CREATE TABLE public.billingmanualdata (
    id uuid NOT NULL,
    regionid smallint NOT NULL,
    createdbyid uuid NOT NULL,
    creationtime timestamp with time zone NOT NULL,
    invoiceid uuid NOT NULL,
    series character varying(50) NOT NULL,
    number character varying(50) NOT NULL
)
PARTITION BY LIST (regionid);


--
-- Name: billingmanualdata_1; Type: TABLE; Schema: public; Owner: admin; Tablespace: region_1
--

CREATE TABLE public.billingmanualdata_1 (
    id uuid NOT NULL,
    regionid smallint NOT NULL,
    createdbyid uuid NOT NULL,
    creationtime timestamp with time zone NOT NULL,
    invoiceid uuid NOT NULL,
    series character varying(50) NOT NULL,
    number character varying(50) NOT NULL,
    CONSTRAINT invoicemanualdata_1_pkey PRIMARY KEY((invoiceid) HASH, regionid ASC, id ASC)
)
SPLIT INTO 6 TABLETS;


--
-- Name: billingpayments; Type: TABLE; Schema: public; Owner: admin
--

CREATE TABLE public.billingpayments (
    id uuid NOT NULL,
    regionid smallint NOT NULL,
    createdbyid uuid NOT NULL,
    creationtime timestamp with time zone NOT NULL,
    invoiceid uuid NOT NULL,
    paymenttransactionid uuid NOT NULL,
    amount numeric(18,4) NOT NULL
)
PARTITION BY LIST (regionid);


--
-- Name: billingpayments_1; Type: TABLE; Schema: public; Owner: admin; Tablespace: region_1
--

CREATE TABLE public.billingpayments_1 (
    id uuid NOT NULL,
    regionid smallint NOT NULL,
    createdbyid uuid NOT NULL,
    creationtime timestamp with time zone NOT NULL,
    invoiceid uuid NOT NULL,
    paymenttransactionid uuid NOT NULL,
    amount numeric(18,4) NOT NULL,
    CONSTRAINT invoicepayments_1_pkey PRIMARY KEY((invoiceid) HASH, regionid ASC, id ASC)
)
SPLIT INTO 6 TABLETS;


--
-- Name: billings; Type: TABLE; Schema: public; Owner: admin
--

CREATE TABLE public.billings (
    id uuid NOT NULL,
    regionid smallint NOT NULL,
    createdbyid uuid NOT NULL,
    creationtime timestamp with time zone NOT NULL,
    lastmodifiedbyid uuid,
    lastmodificationtime timestamp with time zone,
    invoicenumber character varying(50),
    invoicedate timestamp with time zone,
    organizationunitid uuid NOT NULL,
    totalamount numeric(18,4),
    description character varying(255),
    statusid integer DEFAULT 0 NOT NULL,
    supplierorganizationunitid uuid,
    typeid integer NOT NULL,
    currencyid character varying(3),
    holdstatusid bigint,
    signature character varying(2048),
    paymenttermstartdate date,
    financialperiodid uuid,
    originaltotalamount numeric(18,4),
    isprinted boolean,
    fiscalid character varying(20),
    userdataid uuid,
    paymenttermduedate timestamp with time zone,
    printcount integer DEFAULT 0 NOT NULL,
    backendtypeid integer,
    auditingprovider character varying(20),
    chainid uuid,
    sequencenumber bigint,
    previoussignature character varying(2048),
    taxreversecharge boolean DEFAULT false NOT NULL,
    hasunresolveddisputes boolean DEFAULT false NOT NULL,
    calculationmethodid integer DEFAULT 0 NOT NULL,
    stationid uuid,
    expectedtotalamount numeric(16,4),
    backendsystemid character varying(50),
    backendid character varying(50),
    referencedinvoiceid uuid,
    expirationdate timestamp with time zone,
    vatnumber character varying(128),
    subtypeid integer DEFAULT 0 NOT NULL
)
PARTITION BY LIST (regionid);


--
-- Name: billings_1; Type: TABLE; Schema: public; Owner: admin; Tablespace: region_1
--

CREATE TABLE public.billings_1 (
    id uuid NOT NULL,
    regionid smallint NOT NULL,
    createdbyid uuid NOT NULL,
    creationtime timestamp with time zone NOT NULL,
    lastmodifiedbyid uuid,
    lastmodificationtime timestamp with time zone,
    invoicenumber character varying(50),
    invoicedate timestamp with time zone,
    organizationunitid uuid NOT NULL,
    totalamount numeric(18,4),
    description character varying(255),
    statusid integer DEFAULT 0 NOT NULL,
    supplierorganizationunitid uuid,
    typeid integer NOT NULL,
    currencyid character varying(3),
    holdstatusid bigint,
    signature character varying(2048),
    paymenttermstartdate date,
    financialperiodid uuid,
    originaltotalamount numeric(18,4),
    isprinted boolean,
    fiscalid character varying(20),
    userdataid uuid,
    paymenttermduedate timestamp with time zone,
    printcount integer DEFAULT 0 NOT NULL,
    backendtypeid integer,
    auditingprovider character varying(20),
    chainid uuid,
    sequencenumber bigint,
    previoussignature character varying(2048),
    taxreversecharge boolean DEFAULT false NOT NULL,
    hasunresolveddisputes boolean DEFAULT false NOT NULL,
    calculationmethodid integer DEFAULT 0 NOT NULL,
    stationid uuid,
    expectedtotalamount numeric(16,4),
    backendsystemid character varying(50),
    backendid character varying(50),
    referencedinvoiceid uuid,
    expirationdate timestamp with time zone,
    vatnumber character varying(128),
    subtypeid integer DEFAULT 0 NOT NULL,
    CONSTRAINT invoices_1_pkey PRIMARY KEY((id) HASH, regionid ASC)
)
SPLIT INTO 6 TABLETS;


--
-- Name: billinguserdata; Type: TABLE; Schema: public; Owner: admin
--

CREATE TABLE public.billinguserdata (
    id uuid NOT NULL,
    regionid smallint NOT NULL,
    createdbyid uuid NOT NULL,
    creationtime timestamp with time zone NOT NULL,
    emailaddress character varying(100),
    backendrelationid character varying(50),
    initials character varying(20),
    firstname character varying(100),
    lastname character varying(100),
    gender character varying(1),
    fiscalid character varying(20),
    bankaccount character varying(50),
    phonenumber character varying(30),
    billingaddressid uuid,
    companyid uuid,
    userid uuid,
    shippingaddressid uuid,
    organizationunitname character varying(100),
    organizationunitbackendid character varying(50),
    organizationunitaddressid uuid,
    organizationunitbranchnumber character varying(20),
    organizationunitvatnumber character varying(30),
    organizationunitregistrationnumber character varying(128),
    organizationunitcompanyid uuid,
    organizationunitcompanyvatnumber character varying(32),
    countryid character varying(255),
    languageid character varying(255),
    customfields character varying,
    organizationunitcountryid character varying(255),
    organizationunitaddress1 character varying(255),
    organizationunitaddress2 character varying(255),
    organizationunithousenumber character varying(255),
    organizationunitdistrict character varying(255),
    organizationunitsubdistrict character varying(255),
    organizationunitcity character varying(255),
    organizationunitzipcode character varying(255),
    organizationunitstate character varying(255),
    organizationunitemailaddress character varying(255),
    organizationunitphonenumber character varying(255),
    organizationunitwebsite character varying(255),
    organizationunitbic character varying(255),
    organizationunitestablishmentnumber character varying(255),
    lastmodificationtime timestamp with time zone,
    lastmodifiedbyid uuid
)
PARTITION BY LIST (regionid);


--
-- Name: billinguserdata_1; Type: TABLE; Schema: public; Owner: admin; Tablespace: region_1
--

CREATE TABLE public.billinguserdata_1 (
    id uuid NOT NULL,
    regionid smallint NOT NULL,
    createdbyid uuid NOT NULL,
    creationtime timestamp with time zone NOT NULL,
    emailaddress character varying(100),
    backendrelationid character varying(50),
    initials character varying(20),
    firstname character varying(100),
    lastname character varying(100),
    gender character varying(1),
    fiscalid character varying(20),
    bankaccount character varying(50),
    phonenumber character varying(30),
    billingaddressid uuid,
    companyid uuid,
    userid uuid,
    shippingaddressid uuid,
    organizationunitname character varying(100),
    organizationunitbackendid character varying(50),
    organizationunitaddressid uuid,
    organizationunitbranchnumber character varying(20),
    organizationunitvatnumber character varying(30),
    organizationunitregistrationnumber character varying(128),
    organizationunitcompanyid uuid,
    organizationunitcompanyvatnumber character varying(32),
    countryid character varying(255),
    languageid character varying(255),
    customfields character varying,
    organizationunitcountryid character varying(255),
    organizationunitaddress1 character varying(255),
    organizationunitaddress2 character varying(255),
    organizationunithousenumber character varying(255),
    organizationunitdistrict character varying(255),
    organizationunitsubdistrict character varying(255),
    organizationunitcity character varying(255),
    organizationunitzipcode character varying(255),
    organizationunitstate character varying(255),
    organizationunitemailaddress character varying(255),
    organizationunitphonenumber character varying(255),
    organizationunitwebsite character varying(255),
    organizationunitbic character varying(255),
    organizationunitestablishmentnumber character varying(255),
    lastmodificationtime timestamp with time zone,
    lastmodifiedbyid uuid,
    CONSTRAINT invoiceuserdata_1_pkey PRIMARY KEY((id) HASH, regionid ASC)
)
SPLIT INTO 6 TABLETS;


--
-- Name: salesledger; Type: TABLE; Schema: public; Owner: admin
--

CREATE TABLE public.salesledger (
    id uuid NOT NULL,
    regionid smallint NOT NULL,
    orderlineid uuid,
    typeid uuid NOT NULL,
    description character varying(2048),
    createdbyid uuid NOT NULL,
    creationtime timestamp with time zone NOT NULL,
    orderid uuid NOT NULL,
    clientid integer DEFAULT 0,
    oldvalue character varying(255),
    newvalue character varying(255)
)
PARTITION BY LIST (regionid);


--
-- Name: salesledger_1; Type: TABLE; Schema: public; Owner: admin; Tablespace: region_1
--

CREATE TABLE public.salesledger_1 (
    id uuid NOT NULL,
    regionid smallint NOT NULL,
    orderlineid uuid,
    typeid uuid NOT NULL,
    description character varying(2048),
    createdbyid uuid NOT NULL,
    creationtime timestamp with time zone NOT NULL,
    orderid uuid NOT NULL,
    clientid integer DEFAULT 0,
    oldvalue character varying(255),
    newvalue character varying(255),
    CONSTRAINT orderledger_1_pkey PRIMARY KEY((orderid) HASH, regionid ASC, id ASC)
)
SPLIT INTO 6 TABLETS;


--
-- Name: saleslines; Type: TABLE; Schema: public; Owner: admin
--

CREATE TABLE public.saleslines (
    id uuid NOT NULL,
    regionid smallint NOT NULL,
    orderid uuid NOT NULL,
    productid uuid,
    parentid uuid,
    typeid integer NOT NULL,
    description character varying(255) NOT NULL,
    quantityordered integer NOT NULL,
    quantitycancelled integer DEFAULT 0 NOT NULL,
    trackinginformation character varying(255),
    unitprice numeric(18,4) NOT NULL,
    createdbyid uuid NOT NULL,
    creationtime timestamp with time zone NOT NULL,
    lastmodifiedbyid uuid,
    lastmodificationtime timestamp with time zone,
    remark character varying,
    quantityreturned integer DEFAULT 0 NOT NULL,
    stocklabelid uuid NOT NULL,
    iscompleted boolean DEFAULT false NOT NULL,
    unitcost numeric(18,4),
    invoiceid uuid,
    quantityreserved integer DEFAULT 0 NOT NULL,
    serialnumber character varying,
    discountid uuid,
    cardtransactiondate timestamp with time zone,
    cardtransactionid character varying(50),
    cardtransactionstatusid integer,
    contractnumber character varying,
    cardnumber character varying,
    returnedorderlineid uuid,
    ledgerclassid character varying(10),
    lineactiontypeid integer NOT NULL,
    exportstatusid integer DEFAULT 0 NOT NULL,
    expectedavailabilitydate timestamp with time zone,
    trackinglink character varying(2048),
    isdeleted boolean DEFAULT false NOT NULL,
    clientid integer DEFAULT 0,
    quantityexported integer DEFAULT 0 NOT NULL,
    originalunitprice numeric(18,4),
    discountamount numeric(18,4) DEFAULT 0 NOT NULL,
    soldbyid uuid,
    discountpercentage integer,
    invoicedate timestamp with time zone,
    reservationexpirationdate timestamp with time zone,
    discountcouponid uuid,
    organizationunitid uuid,
    shippingmethodid uuid,
    reference character varying(50),
    customorderlinetype character varying(30),
    quantitytoship integer,
    backendreference character varying(255),
    externalmodificationstatusid integer DEFAULT 0 NOT NULL,
    quantityinvoiced integer DEFAULT 0 NOT NULL,
    quantityshipped integer DEFAULT 0 NOT NULL,
    isshipped boolean,
    isinvoiced boolean,
    iscancelled boolean,
    quantityopen integer,
    additionaldescription character varying(255),
    groupid uuid,
    externalsystem character varying(30),
    currencyid character varying(3) NOT NULL,
    exchangerate numeric(16,6) NOT NULL,
    productvariationdata character varying(512),
    taxcodeid uuid NOT NULL,
    optionsid integer DEFAULT 0 NOT NULL,
    relatedorderlineid uuid,
    requesteddate date,
    quantitytoreserve integer DEFAULT 0 NOT NULL,
    costpricecurrencyid character varying(3) NOT NULL,
    productbundlelineid uuid,
    taxexemptioncode character varying(50),
    taxexemptionreason character varying(255),
    quantitydelivered integer DEFAULT 0 NOT NULL,
    commitmentstatusid integer DEFAULT 0 NOT NULL,
    quantitycommitted integer,
    taxrate numeric(18,6) DEFAULT 0 NOT NULL,
    availabilitystatusid integer,
    cancellationreason character varying(255),
    placementstatusid integer DEFAULT 0 NOT NULL,
    ordertypeid integer DEFAULT 0 NOT NULL,
    fulfillmentcalculationdate timestamp with time zone,
    oldreturnreasonid uuid,
    taxamount numeric(16,4),
    discountcouponcode character varying(255),
    unitpricecorrectionreasonid uuid,
    unitpricebeforecorrection numeric(18,4),
    fulfillmentstatusid integer DEFAULT 0 NOT NULL,
    fulfillmentorganizationunitid uuid,
    unitpricepricelistid uuid,
    imageblobid uuid,
    returnedorderid uuid,
    taxratecategoryid integer,
    amountcorrectedafterinvoice numeric(19,5) DEFAULT 0 NOT NULL,
    totalamountintax numeric(18,4) DEFAULT 0 NOT NULL,
    totalamount numeric(18,4) DEFAULT 0 NOT NULL,
    totaltaxamount numeric(18,4) DEFAULT 0 NOT NULL,
    nettotaltaxamount numeric(18,4) DEFAULT 0 NOT NULL,
    nettotalamountintax numeric(18,4) DEFAULT 0 NOT NULL,
    nettotalamount numeric(18,4) DEFAULT 0 NOT NULL,
    unitpriceintax numeric(18,4) DEFAULT 0 NOT NULL,
    unittaxamount numeric(18,4) DEFAULT 0 NOT NULL,
    fulfillmentvisibilitygroupid uuid,
    returnreasonid uuid,
    customstatusid bigint,
    refundcorrectionreasonid uuid,
    ecotax numeric(18,4),
    serialnumberid uuid,
    taxexemptionverification character varying(255),
    amountinvoiced numeric(18,4) DEFAULT 0 NOT NULL,
    discountamountinvoiced numeric(18,4) DEFAULT 0 NOT NULL,
    taxamountinvoiced numeric(18,4) DEFAULT 0 NOT NULL,
    presaledate timestamp with time zone,
    unitpricebeforecorrectionintax numeric(18,4),
    stockproductid uuid,
    appointmentid uuid,
    data text
)
PARTITION BY LIST (regionid);


--
-- Name: saleslines_1; Type: TABLE; Schema: public; Owner: admin; Tablespace: region_1
--

CREATE TABLE public.saleslines_1 (
    id uuid NOT NULL,
    regionid smallint NOT NULL,
    orderid uuid NOT NULL,
    productid uuid,
    parentid uuid,
    typeid integer NOT NULL,
    description character varying(255) NOT NULL,
    quantityordered integer NOT NULL,
    quantitycancelled integer DEFAULT 0 NOT NULL,
    trackinginformation character varying(255),
    unitprice numeric(18,4) NOT NULL,
    createdbyid uuid NOT NULL,
    creationtime timestamp with time zone NOT NULL,
    lastmodifiedbyid uuid,
    lastmodificationtime timestamp with time zone,
    remark character varying,
    quantityreturned integer DEFAULT 0 NOT NULL,
    stocklabelid uuid NOT NULL,
    iscompleted boolean DEFAULT false NOT NULL,
    unitcost numeric(18,4),
    invoiceid uuid,
    quantityreserved integer DEFAULT 0 NOT NULL,
    serialnumber character varying,
    discountid uuid,
    cardtransactiondate timestamp with time zone,
    cardtransactionid character varying(50),
    cardtransactionstatusid integer,
    contractnumber character varying,
    cardnumber character varying,
    returnedorderlineid uuid,
    ledgerclassid character varying(10),
    lineactiontypeid integer NOT NULL,
    exportstatusid integer DEFAULT 0 NOT NULL,
    expectedavailabilitydate timestamp with time zone,
    trackinglink character varying(2048),
    isdeleted boolean DEFAULT false NOT NULL,
    clientid integer DEFAULT 0,
    quantityexported integer DEFAULT 0 NOT NULL,
    originalunitprice numeric(18,4),
    discountamount numeric(18,4) DEFAULT 0 NOT NULL,
    soldbyid uuid,
    discountpercentage integer,
    invoicedate timestamp with time zone,
    reservationexpirationdate timestamp with time zone,
    discountcouponid uuid,
    organizationunitid uuid,
    shippingmethodid uuid,
    reference character varying(50),
    customorderlinetype character varying(30),
    quantitytoship integer,
    backendreference character varying(255),
    externalmodificationstatusid integer DEFAULT 0 NOT NULL,
    quantityinvoiced integer DEFAULT 0 NOT NULL,
    quantityshipped integer DEFAULT 0 NOT NULL,
    isshipped boolean,
    isinvoiced boolean,
    iscancelled boolean,
    quantityopen integer,
    additionaldescription character varying(255),
    groupid uuid,
    externalsystem character varying(30),
    currencyid character varying(3) NOT NULL,
    exchangerate numeric(16,6) NOT NULL,
    productvariationdata character varying(512),
    taxcodeid uuid NOT NULL,
    optionsid integer DEFAULT 0 NOT NULL,
    relatedorderlineid uuid,
    requesteddate date,
    quantitytoreserve integer DEFAULT 0 NOT NULL,
    costpricecurrencyid character varying(3) NOT NULL,
    productbundlelineid uuid,
    taxexemptioncode character varying(50),
    taxexemptionreason character varying(255),
    quantitydelivered integer DEFAULT 0 NOT NULL,
    commitmentstatusid integer DEFAULT 0 NOT NULL,
    quantitycommitted integer,
    taxrate numeric(18,6) DEFAULT 0 NOT NULL,
    availabilitystatusid integer,
    cancellationreason character varying(255),
    placementstatusid integer DEFAULT 0 NOT NULL,
    ordertypeid integer DEFAULT 0 NOT NULL,
    fulfillmentcalculationdate timestamp with time zone,
    oldreturnreasonid uuid,
    taxamount numeric(16,4),
    discountcouponcode character varying(255),
    unitpricecorrectionreasonid uuid,
    unitpricebeforecorrection numeric(18,4),
    fulfillmentstatusid integer DEFAULT 0 NOT NULL,
    fulfillmentorganizationunitid uuid,
    unitpricepricelistid uuid,
    imageblobid uuid,
    returnedorderid uuid,
    taxratecategoryid integer,
    amountcorrectedafterinvoice numeric(19,5) DEFAULT 0 NOT NULL,
    totalamountintax numeric(18,4) DEFAULT 0 NOT NULL,
    totalamount numeric(18,4) DEFAULT 0 NOT NULL,
    totaltaxamount numeric(18,4) DEFAULT 0 NOT NULL,
    nettotaltaxamount numeric(18,4) DEFAULT 0 NOT NULL,
    nettotalamountintax numeric(18,4) DEFAULT 0 NOT NULL,
    nettotalamount numeric(18,4) DEFAULT 0 NOT NULL,
    unitpriceintax numeric(18,4) DEFAULT 0 NOT NULL,
    unittaxamount numeric(18,4) DEFAULT 0 NOT NULL,
    fulfillmentvisibilitygroupid uuid,
    returnreasonid uuid,
    customstatusid bigint,
    refundcorrectionreasonid uuid,
    ecotax numeric(18,4),
    serialnumberid uuid,
    taxexemptionverification character varying(255),
    amountinvoiced numeric(18,4) DEFAULT 0 NOT NULL,
    discountamountinvoiced numeric(18,4) DEFAULT 0 NOT NULL,
    taxamountinvoiced numeric(18,4) DEFAULT 0 NOT NULL,
    presaledate timestamp with time zone,
    unitpricebeforecorrectionintax numeric(18,4),
    stockproductid uuid,
    appointmentid uuid,
    data text,
    CONSTRAINT orderlines_1_pkey PRIMARY KEY((orderid) HASH, regionid ASC, id ASC)
)
SPLIT INTO 6 TABLETS;


--
-- Name: sales; Type: TABLE; Schema: public; Owner: admin
--

CREATE TABLE public.sales (
    id uuid NOT NULL,
    regionid smallint NOT NULL,
    customerid uuid,
    shippingaddressid uuid,
    billingaddressid uuid,
    createdbyid uuid NOT NULL,
    creationtime timestamp with time zone NOT NULL,
    lastmodifiedbyid uuid,
    lastmodificationtime timestamp with time zone,
    sessionid character varying(100),
    iscompleted boolean DEFAULT false NOT NULL,
    remark character varying,
    statusid integer DEFAULT 0 NOT NULL,
    remindertoken character varying,
    ispaid boolean DEFAULT false NOT NULL,
    isinvoiced boolean DEFAULT false NOT NULL,
    isshipped boolean DEFAULT false NOT NULL,
    invoicedate timestamp with time zone,
    invoicedbyid uuid,
    clientid integer DEFAULT 0,
    holdstatusid bigint DEFAULT 0,
    customerreference character varying,
    customerorderid character varying(20),
    customordertype character varying(30),
    characteristicsid integer DEFAULT 0,
    originatingorganizationunitid uuid NOT NULL,
    identificationid uuid,
    currencyid character varying(3) NOT NULL,
    propertiesid integer DEFAULT 0 NOT NULL,
    typeid integer DEFAULT 0 NOT NULL,
    organizationunitid uuid NOT NULL,
    pickuporganizationunitid uuid NOT NULL,
    soldfromorganizationunitid uuid NOT NULL,
    backendid character varying(255),
    backendsystemid character varying(50),
    backendordertype character varying(50),
    shipfromorganizationunitid uuid,
    soldtoorganizationunitid uuid,
    groupid uuid,
    shiptoorganizationunitid uuid,
    description character varying(255),
    customstatusid bigint,
    paymentdate timestamp with time zone,
    shipmentdate timestamp with time zone,
    completiondate timestamp with time zone,
    hasreturns boolean,
    isplaced boolean DEFAULT false NOT NULL,
    placementdate timestamp with time zone,
    useragent character varying(255),
    taxexemptioncode character varying(50),
    taxexemptionreason character varying(255),
    fiscalid character varying(20),
    vatnumber character varying(128),
    prefix character varying(10),
    globalid character varying(22),
    isconfirmed boolean DEFAULT false NOT NULL,
    confirmationdate timestamp with time zone,
    isdelivered boolean DEFAULT false NOT NULL,
    deliverydate timestamp with time zone,
    soldbyid uuid,
    offerexpirationdate timestamp with time zone,
    token character varying(32),
    iscaptured boolean DEFAULT false NOT NULL,
    capturedate timestamp with time zone,
    transferreference character varying(50),
    returntosupplierrequestid uuid,
    fiscalremark character varying(255),
    optionsid integer,
    pickupaddressid uuid,
    totalamountintax numeric(18,4) DEFAULT 0 NOT NULL,
    totalamount numeric(18,4) DEFAULT 0 NOT NULL,
    totaltaxamount numeric(18,4) DEFAULT 0 NOT NULL,
    pickupvisibilitygroupid uuid,
    soldtovisibilitygroupid uuid,
    soldfromvisibilitygroupid uuid,
    shiptovisibilitygroupid uuid,
    shipfromvisibilitygroupid uuid,
    originatingvisibilitygroupid uuid,
    automaticcancellationdate timestamp with time zone,
    customordertypeid uuid,
    clientorderid uuid,
    ordercustomtypeid uuid,
    ordercustomstatusid uuid,
    stationid uuid,
    lastcalculationtime timestamp with time zone,
    createdbyfullname character varying(200),
    taxexemptionverification character varying(255),
    loyaltyprogramvalidated boolean,
    loyaltyprogramid uuid,
    transferredorderid uuid,
    displayid character varying(100),
    serializedorderdata jsonb,
    orderdatasearchkeys text[],
    companyid uuid,
    userproductsubscriptionid uuid,
    languageid character varying(2),
    countryid character varying(3),
    resendreasonid uuid
)
PARTITION BY LIST (regionid);


--
-- Name: sales_1; Type: TABLE; Schema: public; Owner: admin; Tablespace: region_1
--

CREATE TABLE public.sales_1 (
    id uuid NOT NULL,
    regionid smallint NOT NULL,
    customerid uuid,
    shippingaddressid uuid,
    billingaddressid uuid,
    createdbyid uuid NOT NULL,
    creationtime timestamp with time zone NOT NULL,
    lastmodifiedbyid uuid,
    lastmodificationtime timestamp with time zone,
    sessionid character varying(100),
    iscompleted boolean DEFAULT false NOT NULL,
    remark character varying,
    statusid integer DEFAULT 0 NOT NULL,
    remindertoken character varying,
    ispaid boolean DEFAULT false NOT NULL,
    isinvoiced boolean DEFAULT false NOT NULL,
    isshipped boolean DEFAULT false NOT NULL,
    invoicedate timestamp with time zone,
    invoicedbyid uuid,
    clientid integer DEFAULT 0,
    holdstatusid bigint DEFAULT 0,
    customerreference character varying,
    customerorderid character varying(20),
    customordertype character varying(30),
    characteristicsid integer DEFAULT 0,
    originatingorganizationunitid uuid NOT NULL,
    identificationid uuid,
    currencyid character varying(3) NOT NULL,
    propertiesid integer DEFAULT 0 NOT NULL,
    typeid integer DEFAULT 0 NOT NULL,
    organizationunitid uuid NOT NULL,
    pickuporganizationunitid uuid NOT NULL,
    soldfromorganizationunitid uuid NOT NULL,
    backendid character varying(255),
    backendsystemid character varying(50),
    backendordertype character varying(50),
    shipfromorganizationunitid uuid,
    soldtoorganizationunitid uuid,
    groupid uuid,
    shiptoorganizationunitid uuid,
    description character varying(255),
    customstatusid bigint,
    paymentdate timestamp with time zone,
    shipmentdate timestamp with time zone,
    completiondate timestamp with time zone,
    hasreturns boolean,
    isplaced boolean DEFAULT false NOT NULL,
    placementdate timestamp with time zone,
    useragent character varying(255),
    taxexemptioncode character varying(50),
    taxexemptionreason character varying(255),
    fiscalid character varying(20),
    vatnumber character varying(128),
    prefix character varying(10),
    globalid character varying(22),
    isconfirmed boolean DEFAULT false NOT NULL,
    confirmationdate timestamp with time zone,
    isdelivered boolean DEFAULT false NOT NULL,
    deliverydate timestamp with time zone,
    soldbyid uuid,
    offerexpirationdate timestamp with time zone,
    token character varying(32),
    iscaptured boolean DEFAULT false NOT NULL,
    capturedate timestamp with time zone,
    transferreference character varying(50),
    returntosupplierrequestid uuid,
    fiscalremark character varying(255),
    optionsid integer,
    pickupaddressid uuid,
    totalamountintax numeric(18,4) DEFAULT 0 NOT NULL,
    totalamount numeric(18,4) DEFAULT 0 NOT NULL,
    totaltaxamount numeric(18,4) DEFAULT 0 NOT NULL,
    pickupvisibilitygroupid uuid,
    soldtovisibilitygroupid uuid,
    soldfromvisibilitygroupid uuid,
    shiptovisibilitygroupid uuid,
    shipfromvisibilitygroupid uuid,
    originatingvisibilitygroupid uuid,
    automaticcancellationdate timestamp with time zone,
    customordertypeid uuid,
    clientorderid uuid,
    ordercustomtypeid uuid,
    ordercustomstatusid uuid,
    stationid uuid,
    lastcalculationtime timestamp with time zone,
    createdbyfullname character varying(200),
    taxexemptionverification character varying(255),
    loyaltyprogramvalidated boolean,
    loyaltyprogramid uuid,
    transferredorderid uuid,
    displayid character varying(100),
    serializedorderdata jsonb,
    orderdatasearchkeys text[],
    companyid uuid,
    userproductsubscriptionid uuid,
    languageid character varying(2),
    countryid character varying(3),
    resendreasonid uuid,
    CONSTRAINT orders_1_pkey PRIMARY KEY((id) HASH, regionid ASC)
)
SPLIT INTO 6 TABLETS;


--
-- Name: branches; Type: TABLE; Schema: public; Owner: admin
--

CREATE TABLE public.branches (
    id uuid NOT NULL,
    backendid character varying(50),
    name character varying(100) NOT NULL,
    description character varying(255),
    parentid uuid,
    addressid uuid,
    typeid integer NOT NULL,
    lineage character varying(50),
    latitude double precision,
    longitude double precision,
    emailaddress character varying(255),
    phonenumber character varying(30),
    createdbyid uuid NOT NULL,
    creationtime timestamp with time zone NOT NULL,
    lastmodifiedbyid uuid,
    lastmodificationtime timestamp with time zone,
    subnet character varying,
    registercashlimit numeric(18,4) DEFAULT 0,
    safecashlimit numeric(18,4) DEFAULT 0,
    bankaccount character varying(50),
    vatnumber character varying(30),
    backendrelationid character varying(20),
    useforaccounting boolean DEFAULT false NOT NULL,
    backendcompanyid character varying(20),
    branchnumber character varying(20),
    statusid integer DEFAULT 0 NOT NULL,
    isdeleted boolean DEFAULT false NOT NULL,
    attachedtouserid uuid,
    ipaddress character varying(255),
    companyid uuid,
    countryid character varying(3) NOT NULL,
    currencyid character varying(3) NOT NULL,
    globallocationnumber character varying(50),
    accountingorganizationunitid uuid,
    timezone character varying(64),
    costpricecurrencyid character varying(3) NOT NULL,
    languageid character varying(2),
    assortmentid uuid,
    rolesetid uuid NOT NULL,
    cashhandlerid uuid,
    regionid uuid,
    globalid character varying(50),
    registrationnumber character varying(128),
    notes character varying(4000),
    costpricecalculationmethodid integer DEFAULT 0 NOT NULL,
    returnorganizationunitid uuid,
    customfields character varying,
    website character varying(255),
    bic character varying(255),
    auditingfields character varying,
    countrysubdivisionid character varying(10),
    enableendlesssupplierstock boolean DEFAULT false NOT NULL,
    visibilitygroupid uuid,
    establishmentnumber character varying(255),
    cashfloat numeric(18,4),
    taxproviderid uuid,
    isarchived boolean DEFAULT false NOT NULL,
    lastarchivedon timestamp with time zone,
    CONSTRAINT organizationunits_pkey PRIMARY KEY(id ASC)
);


--
-- Name: paymentevents; Type: TABLE; Schema: public; Owner: admin
--

CREATE TABLE public.paymentevents (
    id uuid NOT NULL,
    regionid smallint NOT NULL,
    orderid uuid NOT NULL,
    amount numeric(18,4) NOT NULL,
    paidamount numeric(18,4) NOT NULL,
    paymentdetails character varying,
    errordetails character varying,
    paymentdate timestamp with time zone,
    createdbyid uuid NOT NULL,
    creationtime timestamp with time zone NOT NULL,
    lastmodifiedbyid uuid,
    lastmodificationtime timestamp with time zone,
    backendid character varying(50),
    typeid uuid NOT NULL,
    change numeric(18,4) NOT NULL,
    stationid uuid,
    financialperiodid uuid,
    usercardmutationid uuid,
    backendrelationid character varying(512),
    clientid integer,
    approvedbyid uuid,
    orderlineid uuid,
    returnedpaymenttransactionid uuid,
    statusid integer DEFAULT 0 NOT NULL,
    organizationunitid uuid NOT NULL,
    refundedamount numeric(18,4),
    amounttorefund numeric(18,4),
    deviceid uuid,
    paymentmethod character varying(255),
    currencyid character varying(3) NOT NULL,
    exchangerate numeric(16,6),
    description character varying(255) NOT NULL,
    backendsystemid character varying(50),
    groupid uuid,
    settledamount numeric(18,4) DEFAULT 0 NOT NULL,
    capturedamount numeric(18,4) DEFAULT 0 NOT NULL,
    isrefund boolean DEFAULT false NOT NULL,
    data character varying,
    capturemomentid integer,
    foreigncurrencyid character varying(3),
    foreignamount numeric(18,4),
    foreignpaidamount numeric(18,4),
    amountpendingcapture numeric(18,4) DEFAULT 0 NOT NULL,
    propertiesid integer,
    isdownpayment boolean DEFAULT false NOT NULL,
    shiporderwhenconfirmeddate timestamp with time zone,
    exchangerate2 numeric(18,9),
    isauthorizationadjustment boolean DEFAULT false NOT NULL,
    authorizationadjustmentamount numeric(18,4) DEFAULT 0 NOT NULL,
    amountpendingauthorizationadjustment numeric(18,4) DEFAULT 0 NOT NULL,
    authorizationexpirationdate date,
    dataobjects character varying,
    autocapturedate date
)
PARTITION BY LIST (regionid);


--
-- Name: paymentevents_1; Type: TABLE; Schema: public; Owner: admin; Tablespace: region_1
--

CREATE TABLE public.paymentevents_1 (
    id uuid NOT NULL,
    regionid smallint NOT NULL,
    orderid uuid NOT NULL,
    amount numeric(18,4) NOT NULL,
    paidamount numeric(18,4) NOT NULL,
    paymentdetails character varying,
    errordetails character varying,
    paymentdate timestamp with time zone,
    createdbyid uuid NOT NULL,
    creationtime timestamp with time zone NOT NULL,
    lastmodifiedbyid uuid,
    lastmodificationtime timestamp with time zone,
    backendid character varying(50),
    typeid uuid NOT NULL,
    change numeric(18,4) NOT NULL,
    stationid uuid,
    financialperiodid uuid,
    usercardmutationid uuid,
    backendrelationid character varying(512),
    clientid integer,
    approvedbyid uuid,
    orderlineid uuid,
    returnedpaymenttransactionid uuid,
    statusid integer DEFAULT 0 NOT NULL,
    organizationunitid uuid NOT NULL,
    refundedamount numeric(18,4),
    amounttorefund numeric(18,4),
    deviceid uuid,
    paymentmethod character varying(255),
    currencyid character varying(3) NOT NULL,
    exchangerate numeric(16,6),
    description character varying(255) NOT NULL,
    backendsystemid character varying(50),
    groupid uuid,
    settledamount numeric(18,4) DEFAULT 0 NOT NULL,
    capturedamount numeric(18,4) DEFAULT 0 NOT NULL,
    isrefund boolean DEFAULT false NOT NULL,
    data character varying,
    capturemomentid integer,
    foreigncurrencyid character varying(3),
    foreignamount numeric(18,4),
    foreignpaidamount numeric(18,4),
    amountpendingcapture numeric(18,4) DEFAULT 0 NOT NULL,
    propertiesid integer,
    isdownpayment boolean DEFAULT false NOT NULL,
    shiporderwhenconfirmeddate timestamp with time zone,
    exchangerate2 numeric(18,9),
    isauthorizationadjustment boolean DEFAULT false NOT NULL,
    authorizationadjustmentamount numeric(18,4) DEFAULT 0 NOT NULL,
    amountpendingauthorizationadjustment numeric(18,4) DEFAULT 0 NOT NULL,
    authorizationexpirationdate date,
    dataobjects character varying,
    autocapturedate date,
    CONSTRAINT paymenttransactions_1_pkey PRIMARY KEY((orderid) HASH, regionid ASC, id ASC)
)
SPLIT INTO 6 TABLETS;


--
-- Name: itemledger_1; Type: TABLE; Schema: public; Owner: admin; Tablespace: region_1
--

CREATE TABLE public.itemledger_1 (
    id uuid NOT NULL,
    regionid smallint NOT NULL,
    typeid integer NOT NULL,
    productid uuid NOT NULL,
    organizationunitid uuid NOT NULL,
    quantity integer NOT NULL,
    quantitycommitted integer DEFAULT 0 NOT NULL,
    remark character varying(100),
    stocklabelid uuid NOT NULL,
    issummary boolean NOT NULL,
    stockmutationid uuid,
    orderlineid uuid,
    createdbyid uuid NOT NULL,
    creationtime timestamp with time zone NOT NULL,
    quantityallocated integer DEFAULT 0 NOT NULL,
    CONSTRAINT productledger_1_pkey PRIMARY KEY((id) HASH, regionid ASC)
)
SPLIT INTO 6 TABLETS;


--
-- Name: backgroundjobs; Type: TABLE; Schema: public; Owner: admin
--

CREATE TABLE public.backgroundjobs (
    id character varying(200) NOT NULL,
    name character varying(200) NOT NULL,
    handler character varying(200),
    arguments character varying,
    isrecurring boolean NOT NULL,
    duedate timestamp with time zone,
    cron character varying(100),
    creationtime timestamp with time zone NOT NULL,
    lastmodificationtime timestamp with time zone,
    isdeactivated boolean,
    maxretrycount integer,
    currentretrycount integer,
    minretryinterval time without time zone,
    maxretryinterval time without time zone,
    deltaretryinterval time without time zone,
    lastexecutiontime timestamp with time zone,
    nextexecutiontime timestamp with time zone,
    lastsuccesstime timestamp with time zone,
    lastfailuretime timestamp with time zone,
    lasterror character varying(4000),
    executioncount bigint,
    successcount bigint,
    failurecount bigint,
    delay bigint,
    regionid smallint NOT NULL
)
PARTITION BY LIST (regionid);


--
-- Name: backgroundjobs_1; Type: TABLE; Schema: public; Owner: admin; Tablespace: region_1
--

CREATE TABLE public.backgroundjobs_1 (
    id character varying(200) NOT NULL,
    name character varying(200) NOT NULL,
    handler character varying(200),
    arguments character varying,
    isrecurring boolean NOT NULL,
    duedate timestamp with time zone,
    cron character varying(100),
    creationtime timestamp with time zone NOT NULL,
    lastmodificationtime timestamp with time zone,
    isdeactivated boolean,
    maxretrycount integer,
    currentretrycount integer,
    minretryinterval time without time zone,
    maxretryinterval time without time zone,
    deltaretryinterval time without time zone,
    lastexecutiontime timestamp with time zone,
    nextexecutiontime timestamp with time zone,
    lastsuccesstime timestamp with time zone,
    lastfailuretime timestamp with time zone,
    lasterror character varying(4000),
    executioncount bigint,
    successcount bigint,
    failurecount bigint,
    delay bigint,
    regionid smallint NOT NULL,
    CONSTRAINT scheduledtasks_1_pkey PRIMARY KEY((id) HASH, regionid ASC)
)
SPLIT INTO 6 TABLETS;


--
-- Name: dispatchlines; Type: TABLE; Schema: public; Owner: admin
--

CREATE TABLE public.dispatchlines (
    id uuid NOT NULL,
    regionid smallint NOT NULL,
    createdbyid uuid NOT NULL,
    creationtime timestamp with time zone NOT NULL,
    shipmentid uuid NOT NULL,
    orderlineid uuid NOT NULL,
    quantityshipped integer NOT NULL,
    shipmentdate timestamp with time zone,
    trackingcode character varying(255),
    trackinglink character varying(255),
    quantitydelivered integer,
    backendreference character varying(50),
    iscompleted boolean DEFAULT false NOT NULL,
    orderid uuid NOT NULL,
    lastdeliverydate timestamp with time zone,
    deliverytypeid integer DEFAULT 0 NOT NULL,
    parentid uuid,
    shipmentpackageid uuid,
    orderfulfillmentlineid uuid,
    shippedtounitcost numeric(18,4),
    shippedtocurrencyid character varying(3),
    lastmodifiedbyid uuid,
    lastmodificationtime timestamp with time zone,
    isdeleted boolean DEFAULT false NOT NULL,
    productid uuid
)
PARTITION BY LIST (regionid);


--
-- Name: dispatchlines_1; Type: TABLE; Schema: public; Owner: admin; Tablespace: region_1
--

CREATE TABLE public.dispatchlines_1 (
    id uuid NOT NULL,
    regionid smallint NOT NULL,
    createdbyid uuid NOT NULL,
    creationtime timestamp with time zone NOT NULL,
    shipmentid uuid NOT NULL,
    orderlineid uuid NOT NULL,
    quantityshipped integer NOT NULL,
    shipmentdate timestamp with time zone,
    trackingcode character varying(255),
    trackinglink character varying(255),
    quantitydelivered integer,
    backendreference character varying(50),
    iscompleted boolean DEFAULT false NOT NULL,
    orderid uuid NOT NULL,
    lastdeliverydate timestamp with time zone,
    deliverytypeid integer DEFAULT 0 NOT NULL,
    parentid uuid,
    shipmentpackageid uuid,
    orderfulfillmentlineid uuid,
    shippedtounitcost numeric(18,4),
    shippedtocurrencyid character varying(3),
    lastmodifiedbyid uuid,
    lastmodificationtime timestamp with time zone,
    isdeleted boolean DEFAULT false NOT NULL,
    productid uuid,
    CONSTRAINT shipmentlines_1_pkey PRIMARY KEY((shipmentid) HASH, regionid ASC, id ASC)
)
SPLIT INTO 6 TABLETS;


--
-- Name: dispatches; Type: TABLE; Schema: public; Owner: admin
--

CREATE TABLE public.dispatches (
    id uuid NOT NULL,
    regionid smallint NOT NULL,
    createdbyid uuid NOT NULL,
    creationtime timestamp with time zone NOT NULL,
    lastmodifiedbyid uuid,
    lastmodificationtime timestamp with time zone,
    organizationunitid uuid NOT NULL,
    shipmentdate timestamp with time zone,
    trackingcode character varying(4000),
    trackinglink character varying(4000),
    backendid character varying(128),
    expecteddeliverydate timestamp with time zone,
    backendsystemid character varying(50),
    totalquantityshipped integer DEFAULT 0 NOT NULL,
    totalquantitydelivered integer DEFAULT 0 NOT NULL,
    shippedfromorganizationunitid uuid,
    receivemethodid integer,
    statusid integer DEFAULT 0 NOT NULL,
    ordertypeid integer,
    completiontime timestamp with time zone,
    lastdeliverydate timestamp with time zone,
    isexported boolean DEFAULT false NOT NULL,
    iscompleted boolean,
    nettotalquantityshipped integer DEFAULT 0 NOT NULL,
    nettotalquantitydelivered integer DEFAULT 0 NOT NULL,
    shippingmethodid uuid,
    remark character varying(255),
    optionsid integer NOT NULL,
    isdeleted boolean DEFAULT false NOT NULL,
    exchangerate numeric(18,4),
    exchangerate2 numeric(18,9),
    backendreference character varying(255),
    hasdeviations boolean DEFAULT false NOT NULL,
    shipmentidentifierdata character varying,
    originaldeliverydate timestamp with time zone
)
PARTITION BY LIST (regionid);


--
-- Name: dispatches_1; Type: TABLE; Schema: public; Owner: admin; Tablespace: region_1
--

CREATE TABLE public.dispatches_1 (
    id uuid NOT NULL,
    regionid smallint NOT NULL,
    createdbyid uuid NOT NULL,
    creationtime timestamp with time zone NOT NULL,
    lastmodifiedbyid uuid,
    lastmodificationtime timestamp with time zone,
    organizationunitid uuid NOT NULL,
    shipmentdate timestamp with time zone,
    trackingcode character varying(4000),
    trackinglink character varying(4000),
    backendid character varying(128),
    expecteddeliverydate timestamp with time zone,
    backendsystemid character varying(50),
    totalquantityshipped integer DEFAULT 0 NOT NULL,
    totalquantitydelivered integer DEFAULT 0 NOT NULL,
    shippedfromorganizationunitid uuid,
    receivemethodid integer,
    statusid integer DEFAULT 0 NOT NULL,
    ordertypeid integer,
    completiontime timestamp with time zone,
    lastdeliverydate timestamp with time zone,
    isexported boolean DEFAULT false NOT NULL,
    iscompleted boolean,
    nettotalquantityshipped integer DEFAULT 0 NOT NULL,
    nettotalquantitydelivered integer DEFAULT 0 NOT NULL,
    shippingmethodid uuid,
    remark character varying(255),
    optionsid integer NOT NULL,
    isdeleted boolean DEFAULT false NOT NULL,
    exchangerate numeric(18,4),
    exchangerate2 numeric(18,9),
    backendreference character varying(255),
    hasdeviations boolean DEFAULT false NOT NULL,
    shipmentidentifierdata character varying,
    originaldeliverydate timestamp with time zone,
    CONSTRAINT shipments_1_pkey PRIMARY KEY((id) HASH, regionid ASC)
)
SPLIT INTO 6 TABLETS;


--
-- Name: inventory_1; Type: TABLE; Schema: public; Owner: admin; Tablespace: region_1
--

CREATE TABLE public.inventory_1 (
    organizationunitid uuid NOT NULL,
    productid uuid NOT NULL,
    stocklabelid uuid NOT NULL,
    regionid smallint NOT NULL,
    quantityonhand integer NOT NULL,
    quantityallocated integer NOT NULL,
    CONSTRAINT stock_1_pkey PRIMARY KEY(organizationunitid ASC, productid ASC, stocklabelid ASC, regionid ASC)
);


--
-- Name: accounts; Type: TABLE; Schema: public; Owner: admin
--

CREATE TABLE public.accounts (
    id uuid NOT NULL,
    regionid smallint NOT NULL,
    emailaddress character varying(100),
    firstname character varying(100),
    lastname character varying(100),
    dateofbirth timestamp with time zone,
    gender character varying(1),
    languageid character varying(2) NOT NULL,
    typeid integer NOT NULL,
    shippingaddressid uuid,
    billingaddressid uuid,
    nickname character varying(255),
    phonenumber character varying(30),
    createdbyid uuid NOT NULL,
    creationtime timestamp with time zone NOT NULL,
    lastmodifiedbyid uuid,
    lastmodificationtime timestamp with time zone,
    bankaccount character varying(50),
    backendrelationid character varying(50),
    isdeleted boolean DEFAULT false NOT NULL,
    initials character varying(20),
    placeofbirth character varying(100),
    companyid uuid,
    currentprogressionlevel integer,
    timezone character varying(64),
    fiscalid character varying(20),
    userpropertyid bigint DEFAULT 0 NOT NULL,
    isincognito boolean DEFAULT false NOT NULL,
    backendsystemid character varying(50),
    countryid character varying(3),
    cultureid character varying(6),
    sessionkey character varying(6),
    backendid character varying(128),
    socialsecuritynumber character varying(128),
    useragent character varying(255),
    originid uuid,
    globalid character varying(50),
    isemployee boolean,
    discriminator integer DEFAULT 0 NOT NULL,
    title character varying(100),
    salutation character varying(100),
    preferredstoreid uuid,
    customid character varying(255),
    pricinggroupid character varying(255),
    visibilitygroupid uuid,
    accounttypeid integer DEFAULT 0 NOT NULL,
    deactivationdate timestamp with time zone,
    lastsuccessfullogin timestamp with time zone,
    hasopencases boolean DEFAULT false NOT NULL,
    authenticationid uuid,
    singlesignonid character varying(255),
    originatingorganizationunitid uuid,
    hascompanies boolean DEFAULT false NOT NULL
)
PARTITION BY LIST (regionid);


--
-- Name: accounts_1; Type: TABLE; Schema: public; Owner: admin; Tablespace: region_1
--

CREATE TABLE public.accounts_1 (
    id uuid NOT NULL,
    regionid smallint NOT NULL,
    emailaddress character varying(100),
    firstname character varying(100),
    lastname character varying(100),
    dateofbirth timestamp with time zone,
    gender character varying(1),
    languageid character varying(2) NOT NULL,
    typeid integer NOT NULL,
    shippingaddressid uuid,
    billingaddressid uuid,
    nickname character varying(255),
    phonenumber character varying(30),
    createdbyid uuid NOT NULL,
    creationtime timestamp with time zone NOT NULL,
    lastmodifiedbyid uuid,
    lastmodificationtime timestamp with time zone,
    bankaccount character varying(50),
    backendrelationid character varying(50),
    isdeleted boolean DEFAULT false NOT NULL,
    initials character varying(20),
    placeofbirth character varying(100),
    companyid uuid,
    currentprogressionlevel integer,
    timezone character varying(64),
    fiscalid character varying(20),
    userpropertyid bigint DEFAULT 0 NOT NULL,
    isincognito boolean DEFAULT false NOT NULL,
    backendsystemid character varying(50),
    countryid character varying(3),
    cultureid character varying(6),
    sessionkey character varying(6),
    backendid character varying(128),
    socialsecuritynumber character varying(128),
    useragent character varying(255),
    originid uuid,
    globalid character varying(50),
    isemployee boolean,
    discriminator integer DEFAULT 0 NOT NULL,
    title character varying(100),
    salutation character varying(100),
    preferredstoreid uuid,
    customid character varying(255),
    pricinggroupid character varying(255),
    visibilitygroupid uuid,
    accounttypeid integer DEFAULT 0 NOT NULL,
    deactivationdate timestamp with time zone,
    lastsuccessfullogin timestamp with time zone,
    hasopencases boolean DEFAULT false NOT NULL,
    authenticationid uuid,
    singlesignonid character varying(255),
    originatingorganizationunitid uuid,
    hascompanies boolean DEFAULT false NOT NULL,
    CONSTRAINT users_1_pkey PRIMARY KEY((id) HASH, regionid ASC)
)
SPLIT INTO 6 TABLETS;


--
-- Name: accounttasks; Type: TABLE; Schema: public; Owner: admin
--

CREATE TABLE public.accounttasks (
    id uuid NOT NULL,
    regionid smallint NOT NULL,
    userid uuid,
    typeid uuid NOT NULL,
    priority integer NOT NULL,
    iscompleted boolean NOT NULL,
    isactive boolean NOT NULL,
    createdbyid uuid NOT NULL,
    creationtime timestamp with time zone NOT NULL,
    lastmodifiedbyid uuid,
    lastmodificationtime timestamp with time zone,
    organizationunitid uuid NOT NULL,
    completiontime timestamp with time zone,
    financialperiodid uuid,
    deadline timestamp with time zone,
    expectedtimetocomplete time without time zone,
    description character varying(255),
    starttime timestamp with time zone,
    isignored boolean DEFAULT false NOT NULL,
    subtypeid uuid,
    parentid uuid,
    expectedstarttime timestamp with time zone,
    expectedcompletiontime timestamp with time zone,
    data character varying,
    deactivationreasonid uuid,
    labels character varying,
    ishidden boolean DEFAULT false NOT NULL
)
PARTITION BY LIST (regionid);


--
-- Name: accounttasks_1; Type: TABLE; Schema: public; Owner: admin; Tablespace: region_1
--

CREATE TABLE public.accounttasks_1 (
    id uuid NOT NULL,
    regionid smallint NOT NULL,
    userid uuid,
    typeid uuid NOT NULL,
    priority integer NOT NULL,
    iscompleted boolean NOT NULL,
    isactive boolean NOT NULL,
    createdbyid uuid NOT NULL,
    creationtime timestamp with time zone NOT NULL,
    lastmodifiedbyid uuid,
    lastmodificationtime timestamp with time zone,
    organizationunitid uuid NOT NULL,
    completiontime timestamp with time zone,
    financialperiodid uuid,
    deadline timestamp with time zone,
    expectedtimetocomplete time without time zone,
    description character varying(255),
    starttime timestamp with time zone,
    isignored boolean DEFAULT false NOT NULL,
    subtypeid uuid,
    parentid uuid,
    expectedstarttime timestamp with time zone,
    expectedcompletiontime timestamp with time zone,
    data character varying,
    deactivationreasonid uuid,
    labels character varying,
    ishidden boolean DEFAULT false NOT NULL,
    CONSTRAINT usertasks_1_pkey PRIMARY KEY((id) HASH, regionid ASC)
)
SPLIT INTO 6 TABLETS;


--
-- Name: savedlistdata; Type: TABLE; Schema: public; Owner: admin
--

CREATE TABLE public.savedlistdata (
    id uuid NOT NULL,
    regionid smallint NOT NULL,
    wishlistid uuid,
    type character varying(255) NOT NULL,
    searchkey character varying(128),
    data character varying,
    createdbyid uuid NOT NULL,
    creationtime timestamp with time zone NOT NULL,
    lastmodifiedbyid uuid,
    lastmodificationtime timestamp with time zone
)
PARTITION BY LIST (regionid);


--
-- Name: savedlistdata_1; Type: TABLE; Schema: public; Owner: admin; Tablespace: region_1
--

CREATE TABLE public.savedlistdata_1 (
    id uuid NOT NULL,
    regionid smallint NOT NULL,
    wishlistid uuid,
    type character varying(255) NOT NULL,
    searchkey character varying(128),
    data character varying,
    createdbyid uuid NOT NULL,
    creationtime timestamp with time zone NOT NULL,
    lastmodifiedbyid uuid,
    lastmodificationtime timestamp with time zone,
    CONSTRAINT wishlistdata_1_pkey PRIMARY KEY((id) HASH, regionid ASC)
)
SPLIT INTO 6 TABLETS;


--
-- Name: savedlistlinks; Type: TABLE; Schema: public; Owner: admin
--

CREATE TABLE public.savedlistlinks (
    userid uuid NOT NULL,
    wishlistshareregistrationid uuid NOT NULL,
    wishlistid uuid NOT NULL,
    linkedon timestamp with time zone NOT NULL
);


--
-- Name: savedlistitems; Type: TABLE; Schema: public; Owner: admin
--

CREATE TABLE public.savedlistitems (
    id uuid NOT NULL,
    regionid smallint NOT NULL,
    createdbyid uuid NOT NULL,
    creationtime timestamp with time zone NOT NULL,
    wishlistid uuid NOT NULL,
    productid uuid NOT NULL
)
PARTITION BY LIST (regionid);


--
-- Name: savedlistitems_1; Type: TABLE; Schema: public; Owner: admin; Tablespace: region_1
--

CREATE TABLE public.savedlistitems_1 (
    id uuid NOT NULL,
    regionid smallint NOT NULL,
    createdbyid uuid NOT NULL,
    creationtime timestamp with time zone NOT NULL,
    wishlistid uuid NOT NULL,
    productid uuid NOT NULL,
    CONSTRAINT wishlistproducts_1_pkey PRIMARY KEY((id) HASH, regionid ASC)
)
SPLIT INTO 6 TABLETS;


--
-- Name: savedlists; Type: TABLE; Schema: public; Owner: admin
--

CREATE TABLE public.savedlists (
    id uuid NOT NULL,
    regionid smallint NOT NULL,
    createdbyid uuid NOT NULL,
    creationtime timestamp with time zone NOT NULL,
    lastmodifiedbyid uuid,
    lastmodificationtime timestamp with time zone,
    userid uuid,
    token character varying(200),
    backendsystemid character varying(200),
    backendid character varying(200),
    name character varying(200) NOT NULL,
    description character varying,
    lastreadtime timestamp with time zone,
    createdbyemployee boolean DEFAULT false NOT NULL,
    hidefromuser boolean DEFAULT false NOT NULL,
    itemcount integer DEFAULT 0 NOT NULL,
    firstproductid uuid,
    isdeleted boolean DEFAULT false NOT NULL,
    isshared boolean DEFAULT false NOT NULL
)
PARTITION BY LIST (regionid);


--
-- Name: savedlists_1; Type: TABLE; Schema: public; Owner: admin; Tablespace: region_1
--

CREATE TABLE public.savedlists_1 (
    id uuid NOT NULL,
    regionid smallint NOT NULL,
    createdbyid uuid NOT NULL,
    creationtime timestamp with time zone NOT NULL,
    lastmodifiedbyid uuid,
    lastmodificationtime timestamp with time zone,
    userid uuid,
    token character varying(200),
    backendsystemid character varying(200),
    backendid character varying(200),
    name character varying(200) NOT NULL,
    description character varying,
    lastreadtime timestamp with time zone,
    createdbyemployee boolean DEFAULT false NOT NULL,
    hidefromuser boolean DEFAULT false NOT NULL,
    itemcount integer DEFAULT 0 NOT NULL,
    firstproductid uuid,
    isdeleted boolean DEFAULT false NOT NULL,
    isshared boolean DEFAULT false NOT NULL,
    CONSTRAINT wishlists_1_pkey PRIMARY KEY((id) HASH, regionid ASC)
)
SPLIT INTO 6 TABLETS;


--
-- Name: savedlistshares; Type: TABLE; Schema: public; Owner: admin
--

CREATE TABLE public.savedlistshares (
    id uuid NOT NULL,
    wishlistid uuid NOT NULL,
    description character varying(200) NOT NULL,
    sharedon timestamp with time zone NOT NULL,
    token character varying(100) NOT NULL,
    privilegesid integer NOT NULL,
    isdeleted boolean DEFAULT false NOT NULL,
    CONSTRAINT wishlistshareregistrations_pkey PRIMARY KEY(id ASC)
);


ALTER TABLE ONLY public.locations ATTACH PARTITION public.locations_1 FOR VALUES IN ('1');
ALTER TABLE ONLY public.businesses ATTACH PARTITION public.businesses_1 FOR VALUES IN ('1');
ALTER TABLE ONLY public.pricediscounts ATTACH PARTITION public.pricediscounts_1 FOR VALUES IN ('1');
ALTER TABLE ONLY public.accountingperiods ATTACH PARTITION public.accountingperiods_1 FOR VALUES IN ('1');
ALTER TABLE ONLY public.billinglines ATTACH PARTITION public.billinglines_1 FOR VALUES IN ('1');
ALTER TABLE ONLY public.billingmanualdata ATTACH PARTITION public.billingmanualdata_1 FOR VALUES IN ('1');
ALTER TABLE ONLY public.billingpayments ATTACH PARTITION public.billingpayments_1 FOR VALUES IN ('1');
ALTER TABLE ONLY public.billings ATTACH PARTITION public.billings_1 FOR VALUES IN ('1');
ALTER TABLE ONLY public.billinguserdata ATTACH PARTITION public.billinguserdata_1 FOR VALUES IN ('1');
ALTER TABLE ONLY public.salesledger ATTACH PARTITION public.salesledger_1 FOR VALUES IN ('1');
ALTER TABLE ONLY public.saleslines ATTACH PARTITION public.saleslines_1 FOR VALUES IN ('1');
ALTER TABLE ONLY public.sales ATTACH PARTITION public.sales_1 FOR VALUES IN ('1');
ALTER TABLE ONLY public.paymentevents ATTACH PARTITION public.paymentevents_1 FOR VALUES IN ('1');
ALTER TABLE ONLY public.backgroundjobs ATTACH PARTITION public.backgroundjobs_1 FOR VALUES IN ('1');
ALTER TABLE ONLY public.dispatchlines ATTACH PARTITION public.dispatchlines_1 FOR VALUES IN ('1');
ALTER TABLE ONLY public.dispatches ATTACH PARTITION public.dispatches_1 FOR VALUES IN ('1');
ALTER TABLE ONLY public.accounts ATTACH PARTITION public.accounts_1 FOR VALUES IN ('1');
ALTER TABLE ONLY public.accounttasks ATTACH PARTITION public.accounttasks_1 FOR VALUES IN ('1');
ALTER TABLE ONLY public.savedlistdata ATTACH PARTITION public.savedlistdata_1 FOR VALUES IN ('1');
ALTER TABLE ONLY public.savedlistitems ATTACH PARTITION public.savedlistitems_1 FOR VALUES IN ('1');
ALTER TABLE ONLY public.savedlists ATTACH PARTITION public.savedlists_1 FOR VALUES IN ('1');


--
-- Name: ix_addresses_countryid_zipcode_id_1; Type: INDEX; Schema: public; Owner: admin; Tablespace: region_1
--

CREATE INDEX NONCONCURRENTLY ix_addresses_countryid_zipcode_id_1 ON public.locations_1 USING lsm (((yb_hash_code(id) % 6)) ASC, countryid ASC, zipcode ASC, id DESC) WHERE ((latitude IS NOT NULL) AND (longitude IS NOT NULL));



--
-- Name: ix_addresses_id_hash_1; Type: INDEX; Schema: public; Owner: admin; Tablespace: region_1
--

CREATE INDEX NONCONCURRENTLY ix_addresses_id_hash_1 ON public.locations_1 USING lsm (((yb_hash_code(id) % 6)) ASC, id DESC);



--
-- Name: ix_apikeys_isactive; Type: INDEX; Schema: public; Owner: admin
--

CREATE INDEX NONCONCURRENTLY ix_apikeys_isactive ON public.accesskeys USING lsm (isactive ASC) WHERE (isactive = true);



--
-- Name: ix_apikeys_organizationunitid; Type: INDEX; Schema: public; Owner: admin
--

CREATE INDEX NONCONCURRENTLY ix_apikeys_organizationunitid ON public.accesskeys USING lsm (organizationunitid ASC) WHERE (organizationunitid IS NOT NULL);



--
-- Name: ix_apikeys_temporaryaccesstype; Type: INDEX; Schema: public; Owner: admin
--

CREATE INDEX NONCONCURRENTLY ix_apikeys_temporaryaccesstype ON public.accesskeys USING lsm (temporaryaccesstype ASC) WHERE (temporaryaccesstype IS NOT NULL);



--
-- Name: ix_apikeys_userid; Type: INDEX; Schema: public; Owner: admin
--

CREATE INDEX NONCONCURRENTLY ix_apikeys_userid ON public.accesskeys USING lsm (userid ASC);


--
-- Name: ix_auditchainledger_chainid_creationtime_1; Type: INDEX; Schema: public; Owner: admin; Tablespace: region_1
--

CREATE INDEX NONCONCURRENTLY ix_auditchainledger_chainid_creationtime_1 ON public.eventchainledger_1 USING lsm (((yb_hash_code(id) % 6)) ASC, chainid ASC, creationtime ASC, id ASC);



--
-- Name: ix_auditchainledger_id_hash_asc_1; Type: INDEX; Schema: public; Owner: admin; Tablespace: region_1
--

CREATE INDEX NONCONCURRENTLY ix_auditchainledger_id_hash_asc_1 ON public.eventchainledger_1 USING lsm (((yb_hash_code(id) % 6)) ASC, id ASC);



--
-- Name: ix_companies_backendid_lower_1; Type: INDEX; Schema: public; Owner: admin
--

CREATE INDEX NONCONCURRENTLY ix_companies_backendid_lower_1 ON public.businesses_1 USING lsm (lower((backendid)::text) HASH) INCLUDE (isdeleted) SPLIT INTO 6 TABLETS WHERE (backendid IS NOT NULL);


--
-- Name: ix_companies_id_hash_1; Type: INDEX; Schema: public; Owner: admin; Tablespace: region_1
--

CREATE INDEX NONCONCURRENTLY ix_companies_id_hash_1 ON public.businesses_1 USING lsm (((yb_hash_code(id) % 6)) ASC, id DESC);



--
-- Name: ix_companies_originatingid_typeid_1; Type: INDEX; Schema: public; Owner: admin; Tablespace: region_1
--

CREATE INDEX NONCONCURRENTLY ix_companies_originatingid_typeid_1 ON public.businesses_1 USING lsm (((yb_hash_code(id) % 6)) ASC, originatingid ASC, typeid ASC, id DESC) INCLUDE (name, backendid, isdeleted);


--
-- Name: ix_companies_vatnumber_1; Type: INDEX; Schema: public; Owner: admin
--

CREATE INDEX NONCONCURRENTLY ix_companies_vatnumber_1 ON public.businesses_1 USING lsm (vatnumber HASH) SPLIT INTO 6 TABLETS WHERE (vatnumber IS NOT NULL);



--
-- Name: ix_discountdetails_discountid_1; Type: INDEX; Schema: public; Owner: admin
--

CREATE INDEX NONCONCURRENTLY ix_discountdetails_discountid_1 ON public.pricediscounts_1 USING lsm (discountid HASH) SPLIT INTO 6 TABLETS;



--
-- Name: ix_discountdetails_discountorderlineid_1; Type: INDEX; Schema: public; Owner: admin
--

CREATE INDEX NONCONCURRENTLY ix_discountdetails_discountorderlineid_1 ON public.pricediscounts_1 USING lsm (discountorderlineid HASH) SPLIT INTO 6 TABLETS;



--
-- Name: ix_discountdetails_parentorderlineid_1; Type: INDEX; Schema: public; Owner: admin
--

CREATE INDEX NONCONCURRENTLY ix_discountdetails_parentorderlineid_1 ON public.pricediscounts_1 USING lsm (parentorderlineid HASH) SPLIT INTO 6 TABLETS;


--
-- Name: ix_financialperiods_id_hash_1; Type: INDEX; Schema: public; Owner: admin; Tablespace: region_1
--

CREATE INDEX NONCONCURRENTLY ix_financialperiods_id_hash_1 ON public.accountingperiods_1 USING lsm (((yb_hash_code(id) % 6)) ASC, id DESC);


--
-- Name: ix_financialperiods_open_periods_1; Type: INDEX; Schema: public; Owner: admin
--

CREATE INDEX NONCONCURRENTLY ix_financialperiods_open_periods_1 ON public.accountingperiods_1 USING lsm (organizationunitid HASH) SPLIT INTO 6 TABLETS WHERE (statusid < 2);



--
-- Name: ix_financialperiods_organizationunitid_1; Type: INDEX; Schema: public; Owner: admin
--

CREATE INDEX NONCONCURRENTLY ix_financialperiods_organizationunitid_1 ON public.accountingperiods_1 USING lsm (organizationunitid HASH) SPLIT INTO 6 TABLETS;



--
-- Name: ix_financialperiods_previousperiodid_1; Type: INDEX; Schema: public; Owner: admin
--

CREATE INDEX NONCONCURRENTLY ix_financialperiods_previousperiodid_1 ON public.accountingperiods_1 USING lsm (previousperiodid HASH) SPLIT INTO 6 TABLETS WHERE (previousperiodid IS NOT NULL);


--
-- Name: ix_financialperiods_statusid_1; Type: INDEX; Schema: public; Owner: admin; Tablespace: region_1
--

CREATE INDEX NONCONCURRENTLY ix_financialperiods_statusid_1 ON public.accountingperiods_1 USING lsm (((yb_hash_code(id) % 6)) ASC, statusid DESC, id DESC) INCLUDE (openingtime, closingtime, organizationunitid);


--
-- Name: ix_financialperiods_userid_1; Type: INDEX; Schema: public; Owner: admin
--

CREATE INDEX NONCONCURRENTLY ix_financialperiods_userid_1 ON public.accountingperiods_1 USING lsm (userid HASH) SPLIT INTO 6 TABLETS WHERE (userid IS NOT NULL);



--
-- Name: ix_invoicelines_creationtime_1; Type: INDEX; Schema: public; Owner: admin
--

CREATE INDEX NONCONCURRENTLY ix_invoicelines_creationtime_1 ON public.billinglines_1 USING lsm (creationtime ASC) INCLUDE (orderlineid);



--
-- Name: ix_invoicelines_invoicedate_1; Type: INDEX; Schema: public; Owner: admin
--

CREATE INDEX NONCONCURRENTLY ix_invoicelines_invoicedate_1 ON public.billinglines_1 USING lsm (invoicedate ASC) INCLUDE (orderlineid) WHERE (invoicedate IS NOT NULL);



--
-- Name: ix_invoicelines_orderid_1; Type: INDEX; Schema: public; Owner: admin
--

CREATE INDEX NONCONCURRENTLY ix_invoicelines_orderid_1 ON public.billinglines_1 USING lsm (orderid HASH) INCLUDE (invoiceid) SPLIT INTO 6 TABLETS WHERE (orderid IS NOT NULL);



--
-- Name: ix_invoicelines_orderlineid_1; Type: INDEX; Schema: public; Owner: admin
--

CREATE INDEX NONCONCURRENTLY ix_invoicelines_orderlineid_1 ON public.billinglines_1 USING lsm (orderlineid HASH) INCLUDE (invoicedate) SPLIT INTO 6 TABLETS;



--
-- Name: ix_invoicelines_shipmentlineid_1; Type: INDEX; Schema: public; Owner: admin
--

CREATE INDEX NONCONCURRENTLY ix_invoicelines_shipmentlineid_1 ON public.billinglines_1 USING lsm (shipmentlineid HASH) SPLIT INTO 6 TABLETS WHERE (shipmentlineid IS NOT NULL);



--
-- Name: ix_invoicepayments_paymenttransactionid_1; Type: INDEX; Schema: public; Owner: admin
--

CREATE INDEX NONCONCURRENTLY ix_invoicepayments_paymenttransactionid_1 ON public.billingpayments_1 USING lsm (paymenttransactionid HASH) SPLIT INTO 6 TABLETS;



--
-- Name: ix_invoices_financialperiodid_1; Type: INDEX; Schema: public; Owner: admin
--

CREATE INDEX NONCONCURRENTLY ix_invoices_financialperiodid_1 ON public.billings_1 USING lsm (financialperiodid HASH) SPLIT INTO 6 TABLETS WHERE (financialperiodid IS NOT NULL);


--
-- Name: ix_invoices_id_hash_1; Type: INDEX; Schema: public; Owner: admin; Tablespace: region_1
--

CREATE INDEX NONCONCURRENTLY ix_invoices_id_hash_1 ON public.billings_1 USING lsm (((yb_hash_code(id) % 6)) ASC, id DESC);



--
-- Name: ix_invoices_invoicedate_1; Type: INDEX; Schema: public; Owner: admin; Tablespace: region_1
--

CREATE INDEX NONCONCURRENTLY ix_invoices_invoicedate_1 ON public.billings_1 USING lsm (((yb_hash_code(id) % 6)) ASC, invoicedate ASC) WHERE (invoicedate IS NOT NULL);


--
-- Name: ix_invoices_invoicenumber_1; Type: INDEX; Schema: public; Owner: admin
--

CREATE INDEX NONCONCURRENTLY ix_invoices_invoicenumber_1 ON public.billings_1 USING lsm (invoicenumber HASH) SPLIT INTO 6 TABLETS WHERE (invoicenumber IS NOT NULL);



--
-- Name: ix_invoices_lastmodifiedbyid_1; Type: INDEX; Schema: public; Owner: admin
--

CREATE INDEX NONCONCURRENTLY ix_invoices_lastmodifiedbyid_1 ON public.billings_1 USING lsm (lastmodifiedbyid HASH) SPLIT INTO 6 TABLETS WHERE (lastmodifiedbyid IS NOT NULL);


--
-- Name: ix_invoices_organizationunitid_chainid_stationid_1; Type: INDEX; Schema: public; Owner: admin; Tablespace: region_1
--

CREATE INDEX NONCONCURRENTLY ix_invoices_organizationunitid_chainid_stationid_1 ON public.billings_1 USING lsm (((yb_hash_code(id) % 6)) ASC, organizationunitid DESC, chainid DESC, stationid DESC, id DESC);


--
-- Name: ix_invoices_referencedinvoiceid_1; Type: INDEX; Schema: public; Owner: admin
--

CREATE INDEX NONCONCURRENTLY ix_invoices_referencedinvoiceid_1 ON public.billings_1 USING lsm (referencedinvoiceid HASH) SPLIT INTO 6 TABLETS WHERE (referencedinvoiceid IS NOT NULL);



--
-- Name: ix_orderledger_orderlineid_1; Type: INDEX; Schema: public; Owner: admin
--

CREATE INDEX NONCONCURRENTLY ix_orderledger_orderlineid_1 ON public.salesledger_1 USING lsm (orderlineid HASH) SPLIT INTO 6 TABLETS WHERE (orderlineid IS NOT NULL);



--
-- Name: ix_orderlines_backendreference_1; Type: INDEX; Schema: public; Owner: admin
--

CREATE INDEX NONCONCURRENTLY ix_orderlines_backendreference_1 ON public.saleslines_1 USING lsm (lower((backendreference)::text) HASH) SPLIT INTO 6 TABLETS WHERE (backendreference IS NOT NULL);



--
-- Name: ix_orderlines_commitmentstatusid_1; Type: INDEX; Schema: public; Owner: admin
--

CREATE INDEX NONCONCURRENTLY ix_orderlines_commitmentstatusid_1 ON public.saleslines_1 USING lsm (commitmentstatusid ASC);


--
-- Name: ix_orderlines_creationtime_1; Type: INDEX; Schema: public; Owner: admin; Tablespace: region_1
--

CREATE INDEX NONCONCURRENTLY ix_orderlines_creationtime_1 ON public.saleslines_1 USING lsm (((yb_hash_code(id) % 6)) ASC, creationtime ASC);


--
-- Name: ix_orderlines_creationtime_lastmodificationtime_1; Type: INDEX; Schema: public; Owner: admin
--

CREATE INDEX NONCONCURRENTLY ix_orderlines_creationtime_lastmodificationtime_1 ON public.saleslines_1 USING lsm (creationtime ASC, lastmodificationtime ASC);


--
-- Name: ix_orderlines_creationtime_quantityordered_1; Type: INDEX; Schema: public; Owner: admin; Tablespace: region_1
--

CREATE INDEX NONCONCURRENTLY ix_orderlines_creationtime_quantityordered_1 ON public.saleslines_1 USING lsm (((yb_hash_code(id) % 6)) ASC, creationtime ASC, quantityordered ASC) INCLUDE (orderid) WHERE ((iscompleted = false) AND (isdeleted = false) AND (lineactiontypeid = 2) AND (placementstatusid = 4));


--
-- Name: ix_orderlines_discountcouponid_1; Type: INDEX; Schema: public; Owner: admin
--

CREATE INDEX NONCONCURRENTLY ix_orderlines_discountcouponid_1 ON public.saleslines_1 USING lsm (discountcouponid HASH) SPLIT INTO 6 TABLETS WHERE (discountcouponid IS NOT NULL);



--
-- Name: ix_orderlines_discountid_1; Type: INDEX; Schema: public; Owner: admin
--

CREATE INDEX NONCONCURRENTLY ix_orderlines_discountid_1 ON public.saleslines_1 USING lsm (discountid HASH) SPLIT INTO 6 TABLETS WHERE (discountid IS NOT NULL);



--
-- Name: ix_orderlines_expectedfuturestock_1; Type: INDEX; Schema: public; Owner: admin
--

CREATE INDEX NONCONCURRENTLY ix_orderlines_expectedfuturestock_1 ON public.saleslines_1 USING lsm (ordertypeid HASH, productid ASC, requesteddate ASC, quantitytoship ASC, quantitydelivered ASC) SPLIT INTO 6 TABLETS WHERE ((ordertypeid = 1) AND (productid IS NOT NULL) AND (requesteddate IS NOT NULL) AND (quantitydelivered = 0) AND (quantitytoship > 0));



--
-- Name: ix_orderlines_groupid_1; Type: INDEX; Schema: public; Owner: admin
--

CREATE INDEX NONCONCURRENTLY ix_orderlines_groupid_1 ON public.saleslines_1 USING lsm (groupid HASH) SPLIT INTO 6 TABLETS WHERE (groupid IS NOT NULL);


--
-- Name: ix_orderlines_id_hash_1; Type: INDEX; Schema: public; Owner: admin; Tablespace: region_1
--

CREATE INDEX NONCONCURRENTLY ix_orderlines_id_hash_1 ON public.saleslines_1 USING lsm (((yb_hash_code(id) % 6)) ASC, id DESC);


--
-- Name: ix_orderlines_invoiceid_1; Type: INDEX; Schema: public; Owner: admin
--

CREATE INDEX NONCONCURRENTLY ix_orderlines_invoiceid_1 ON public.saleslines_1 USING lsm (invoiceid HASH) SPLIT INTO 6 TABLETS WHERE (invoiceid IS NOT NULL);


--
-- Name: ix_orderlines_presaledate_1; Type: INDEX; Schema: public; Owner: admin; Tablespace: region_1
--

CREATE INDEX NONCONCURRENTLY ix_orderlines_presaledate_1 ON public.saleslines_1 USING lsm (presaledate ASC) WHERE (presaledate IS NOT NULL);



--
-- Name: ix_orderlines_productid_1; Type: INDEX; Schema: public; Owner: admin; Tablespace: region_1
--

CREATE INDEX NONCONCURRENTLY ix_orderlines_productid_1 ON public.saleslines_1 USING lsm (((yb_hash_code(id) % 6)) ASC, productid DESC, id DESC) WHERE (productid IS NOT NULL);


--
-- Name: ix_orderlines_relatedorderlineid_1; Type: INDEX; Schema: public; Owner: admin
--

CREATE INDEX NONCONCURRENTLY ix_orderlines_relatedorderlineid_1 ON public.saleslines_1 USING lsm (relatedorderlineid HASH) SPLIT INTO 6 TABLETS WHERE (relatedorderlineid IS NOT NULL);



--
-- Name: ix_orderlines_reservationexpirationdate_1; Type: INDEX; Schema: public; Owner: admin
--

CREATE INDEX NONCONCURRENTLY ix_orderlines_reservationexpirationdate_1 ON public.saleslines_1 USING lsm (reservationexpirationdate ASC) WHERE (reservationexpirationdate IS NOT NULL);



--
-- Name: ix_orderlines_returnedorderid_orderid_1; Type: INDEX; Schema: public; Owner: admin
--

CREATE INDEX NONCONCURRENTLY ix_orderlines_returnedorderid_orderid_1 ON public.saleslines_1 USING lsm (returnedorderid HASH, orderid ASC) SPLIT INTO 6 TABLETS WHERE (returnedorderid IS NOT NULL);



--
-- Name: ix_orderlines_returnedorderlineid_1; Type: INDEX; Schema: public; Owner: admin
--

CREATE INDEX NONCONCURRENTLY ix_orderlines_returnedorderlineid_1 ON public.saleslines_1 USING lsm (returnedorderlineid HASH) INCLUDE (orderid) SPLIT INTO 6 TABLETS WHERE (returnedorderlineid IS NOT NULL);



--
-- Name: ix_orders_automaticcancellationdate_1; Type: INDEX; Schema: public; Owner: admin
--

CREATE INDEX NONCONCURRENTLY ix_orders_automaticcancellationdate_1 ON public.sales_1 USING lsm (automaticcancellationdate ASC) WHERE (automaticcancellationdate IS NOT NULL);



--
-- Name: ix_orders_backendid_backendsystemid_1; Type: INDEX; Schema: public; Owner: admin
--

CREATE INDEX NONCONCURRENTLY ix_orders_backendid_backendsystemid_1 ON public.sales_1 USING lsm (lower((backendid)::text) HASH, backendsystemid ASC) SPLIT INTO 6 TABLETS WHERE (backendid IS NOT NULL);



--
-- Name: ix_orders_companyid_1; Type: INDEX; Schema: public; Owner: admin
--

CREATE INDEX NONCONCURRENTLY ix_orders_companyid_1 ON public.sales_1 USING lsm (companyid HASH) SPLIT INTO 6 TABLETS WHERE (companyid IS NOT NULL);


--
-- Name: ix_orders_creationtime_id_1; Type: INDEX; Schema: public; Owner: admin; Tablespace: region_1
--

CREATE INDEX NONCONCURRENTLY ix_orders_creationtime_id_1 ON public.sales_1 USING lsm (((yb_hash_code(id) % 6)) ASC, creationtime ASC, id ASC);


--
-- Name: ix_orders_creationtime_lastmodificationtime_1; Type: INDEX; Schema: public; Owner: admin
--

CREATE INDEX NONCONCURRENTLY ix_orders_creationtime_lastmodificationtime_1 ON public.sales_1 USING lsm (creationtime ASC, lastmodificationtime ASC);



--
-- Name: ix_orders_customerid_1; Type: INDEX; Schema: public; Owner: admin
--

CREATE INDEX NONCONCURRENTLY ix_orders_customerid_1 ON public.sales_1 USING lsm (customerid HASH) SPLIT INTO 6 TABLETS WHERE (customerid IS NOT NULL);



--
-- Name: ix_orders_displayid_1; Type: INDEX; Schema: public; Owner: admin
--

CREATE INDEX NONCONCURRENTLY ix_orders_displayid_1 ON public.sales_1 USING lsm (displayid HASH) SPLIT INTO 6 TABLETS WHERE (displayid IS NOT NULL);



--
-- Name: ix_orders_globalid_1; Type: INDEX; Schema: public; Owner: admin
--

CREATE INDEX NONCONCURRENTLY ix_orders_globalid_1 ON public.sales_1 USING lsm (globalid HASH) SPLIT INTO 6 TABLETS;



--
-- Name: ix_orders_groupid_1; Type: INDEX; Schema: public; Owner: admin
--

CREATE INDEX NONCONCURRENTLY ix_orders_groupid_1 ON public.sales_1 USING lsm (groupid HASH) SPLIT INTO 6 TABLETS WHERE (groupid IS NOT NULL);


--
-- Name: ix_orders_id_hash_1; Type: INDEX; Schema: public; Owner: admin; Tablespace: region_1
--

CREATE INDEX NONCONCURRENTLY ix_orders_id_hash_1 ON public.sales_1 USING lsm (((yb_hash_code(id) % 6)) ASC, id DESC);



--
-- Name: ix_organizationunits_addressid; Type: INDEX; Schema: public; Owner: admin
--

CREATE INDEX NONCONCURRENTLY ix_organizationunits_addressid ON public.branches USING lsm (addressid ASC) WHERE (addressid IS NOT NULL);



--
-- Name: ix_organizationunits_isdeleted; Type: INDEX; Schema: public; Owner: admin
--

CREATE INDEX NONCONCURRENTLY ix_organizationunits_isdeleted ON public.branches USING lsm (isdeleted ASC);



--
-- Name: ix_organizationunits_parentid; Type: INDEX; Schema: public; Owner: admin
--

CREATE INDEX NONCONCURRENTLY ix_organizationunits_parentid ON public.branches USING lsm (parentid ASC) WHERE (parentid IS NOT NULL);



--
-- Name: ix_organizationunits_typeid; Type: INDEX; Schema: public; Owner: admin
--

CREATE INDEX NONCONCURRENTLY ix_organizationunits_typeid ON public.branches USING lsm (typeid ASC);



--
-- Name: ix_paymenttransactions_authorizationexpirationdate_1; Type: INDEX; Schema: public; Owner: admin
--

CREATE INDEX NONCONCURRENTLY ix_paymenttransactions_authorizationexpirationdate_1 ON public.paymentevents_1 USING lsm (authorizationexpirationdate HASH) SPLIT INTO 6 TABLETS WHERE (authorizationexpirationdate IS NOT NULL);



--
-- Name: ix_paymenttransactions_autocapturedate_1; Type: INDEX; Schema: public; Owner: admin
--

CREATE INDEX NONCONCURRENTLY ix_paymenttransactions_autocapturedate_1 ON public.paymentevents_1 USING lsm (autocapturedate HASH) SPLIT INTO 6 TABLETS WHERE (autocapturedate IS NOT NULL);



--
-- Name: ix_paymenttransactions_backendid_1; Type: INDEX; Schema: public; Owner: admin
--

CREATE INDEX NONCONCURRENTLY ix_paymenttransactions_backendid_1 ON public.paymentevents_1 USING lsm (backendid HASH) SPLIT INTO 6 TABLETS WHERE (backendid IS NOT NULL);



--
-- Name: ix_paymenttransactions_creationtime_1; Type: INDEX; Schema: public; Owner: admin
--

CREATE INDEX NONCONCURRENTLY ix_paymenttransactions_creationtime_1 ON public.paymentevents_1 USING lsm (creationtime ASC) INCLUDE (stationid);



--
-- Name: ix_paymenttransactions_creationtime_lastmodificationtime_1; Type: INDEX; Schema: public; Owner: admin
--

CREATE INDEX NONCONCURRENTLY ix_paymenttransactions_creationtime_lastmodificationtime_1 ON public.paymentevents_1 USING lsm (creationtime ASC, lastmodificationtime ASC);



--
-- Name: ix_paymenttransactions_financialperiodid_1; Type: INDEX; Schema: public; Owner: admin
--

CREATE INDEX NONCONCURRENTLY ix_paymenttransactions_financialperiodid_1 ON public.paymentevents_1 USING lsm (financialperiodid HASH) SPLIT INTO 6 TABLETS WHERE (financialperiodid IS NOT NULL);



--
-- Name: ix_paymenttransactions_groupid_1; Type: INDEX; Schema: public; Owner: admin
--

CREATE INDEX NONCONCURRENTLY ix_paymenttransactions_groupid_1 ON public.paymentevents_1 USING lsm (groupid HASH) SPLIT INTO 6 TABLETS WHERE (groupid IS NOT NULL);


--
-- Name: ix_paymenttransactions_id_hash_1; Type: INDEX; Schema: public; Owner: admin; Tablespace: region_1
--

CREATE INDEX NONCONCURRENTLY ix_paymenttransactions_id_hash_1 ON public.paymentevents_1 USING lsm (((yb_hash_code(id) % 6)) ASC, id DESC);



--
-- Name: ix_paymenttransactions_organizationunitid_1; Type: INDEX; Schema: public; Owner: admin; Tablespace: region_1
--

CREATE INDEX NONCONCURRENTLY ix_paymenttransactions_organizationunitid_1 ON public.paymentevents_1 USING lsm (((yb_hash_code(id) % 6)) ASC, organizationunitid DESC, id DESC);


--
-- Name: ix_paymenttransactions_paymentmethod_backendrelationid_1; Type: INDEX; Schema: public; Owner: admin
--

CREATE INDEX NONCONCURRENTLY ix_paymenttransactions_paymentmethod_backendrelationid_1 ON public.paymentevents_1 USING lsm (paymentmethod HASH, backendrelationid ASC) SPLIT INTO 6 TABLETS WHERE (paymentmethod IS NOT NULL);



--
-- Name: ix_paymenttransactions_stationid_1; Type: INDEX; Schema: public; Owner: admin
--

CREATE INDEX NONCONCURRENTLY ix_paymenttransactions_stationid_1 ON public.paymentevents_1 USING lsm (stationid HASH) SPLIT INTO 6 TABLETS WHERE (stationid IS NOT NULL);


--
-- Name: ix_paymenttransactions_typeid_1; Type: INDEX; Schema: public; Owner: admin; Tablespace: region_1
--

CREATE INDEX NONCONCURRENTLY ix_paymenttransactions_typeid_1 ON public.paymentevents_1 USING lsm (((yb_hash_code(id) % 6)) ASC, typeid DESC, id DESC);


--
-- Name: ix_paymenttransactions_usercardmutationid_1; Type: INDEX; Schema: public; Owner: admin
--

CREATE INDEX NONCONCURRENTLY ix_paymenttransactions_usercardmutationid_1 ON public.paymentevents_1 USING lsm (usercardmutationid HASH) SPLIT INTO 6 TABLETS WHERE (usercardmutationid IS NOT NULL);



--
-- Name: ix_productid_1; Type: INDEX; Schema: public; Owner: admin
--

CREATE INDEX NONCONCURRENTLY ix_productid_1 ON public.savedlistitems_1 USING lsm (productid HASH) SPLIT INTO 6 TABLETS;



--
-- Name: ix_productledger_creationtime_1; Type: INDEX; Schema: public; Owner: admin
--

CREATE INDEX NONCONCURRENTLY ix_productledger_creationtime_1 ON public.itemledger_1 USING lsm (creationtime ASC) INCLUDE (productid);


--
-- Name: ix_productledger_id_hash_1; Type: INDEX; Schema: public; Owner: admin; Tablespace: region_1
--

CREATE INDEX NONCONCURRENTLY ix_productledger_id_hash_1 ON public.itemledger_1 USING lsm (((yb_hash_code(id) % 6)) ASC, id DESC);



--
-- Name: ix_productledger_organizationunitid_1; Type: INDEX; Schema: public; Owner: admin; Tablespace: region_1
--

CREATE INDEX NONCONCURRENTLY ix_productledger_organizationunitid_1 ON public.itemledger_1 USING lsm (((yb_hash_code(id) % 6)) ASC, organizationunitid DESC, id DESC) INCLUDE (creationtime, orderlineid, productid);


--
-- Name: ix_productledger_productid_1; Type: INDEX; Schema: public; Owner: admin
--

CREATE INDEX NONCONCURRENTLY ix_productledger_productid_1 ON public.itemledger_1 USING lsm (productid HASH) SPLIT INTO 6 TABLETS;



--
-- Name: ix_scheduledtasks_isdeactivated_1; Type: INDEX; Schema: public; Owner: admin
--

CREATE INDEX NONCONCURRENTLY ix_scheduledtasks_isdeactivated_1 ON public.backgroundjobs_1 USING lsm (isdeactivated ASC) WHERE (isdeactivated = true);



--
-- Name: ix_scheduledtasks_isrecurring_1; Type: INDEX; Schema: public; Owner: admin
--

CREATE INDEX NONCONCURRENTLY ix_scheduledtasks_isrecurring_1 ON public.backgroundjobs_1 USING lsm (isrecurring ASC) WHERE (isrecurring = true);



--
-- Name: ix_scheduledtasks_isrecurring_nextexecutiontime_1; Type: INDEX; Schema: public; Owner: admin
--

CREATE INDEX NONCONCURRENTLY ix_scheduledtasks_isrecurring_nextexecutiontime_1 ON public.backgroundjobs_1 USING lsm (isrecurring ASC, nextexecutiontime ASC) WHERE (isrecurring = false);


--
-- Name: ix_shipmentlines_id_hash_1; Type: INDEX; Schema: public; Owner: admin; Tablespace: region_1
--

CREATE INDEX NONCONCURRENTLY ix_shipmentlines_id_hash_1 ON public.dispatchlines_1 USING lsm (((yb_hash_code(id) % 6)) ASC, id DESC);


--
-- Name: ix_shipmentlines_orderid_1; Type: INDEX; Schema: public; Owner: admin
--

CREATE INDEX NONCONCURRENTLY ix_shipmentlines_orderid_1 ON public.dispatchlines_1 USING lsm (orderid HASH) INCLUDE (shipmentid) SPLIT INTO 6 TABLETS;



--
-- Name: ix_shipmentlines_orderlineid_1; Type: INDEX; Schema: public; Owner: admin
--

CREATE INDEX NONCONCURRENTLY ix_shipmentlines_orderlineid_1 ON public.dispatchlines_1 USING lsm (orderlineid HASH) SPLIT INTO 6 TABLETS;



--
-- Name: ix_shipmentlines_parentid_isdeleted_1; Type: INDEX; Schema: public; Owner: admin
--

CREATE INDEX NONCONCURRENTLY ix_shipmentlines_parentid_isdeleted_1 ON public.dispatchlines_1 USING lsm (parentid HASH, isdeleted ASC) SPLIT INTO 6 TABLETS WHERE (parentid IS NOT NULL);



--
-- Name: ix_shipmentlines_quantityshipped_1; Type: INDEX; Schema: public; Owner: admin
--

CREATE INDEX NONCONCURRENTLY ix_shipmentlines_quantityshipped_1 ON public.dispatchlines_1 USING lsm (quantityshipped ASC) INCLUDE (parentid, isdeleted);


--
-- Name: ix_shipments_backendid_1; Type: INDEX; Schema: public; Owner: admin
--

CREATE INDEX NONCONCURRENTLY ix_shipments_backendid_1 ON public.dispatches_1 USING lsm (backendid HASH) SPLIT INTO 6 TABLETS WHERE (backendid IS NOT NULL);



--
-- Name: ix_shipments_backendreference_1; Type: INDEX; Schema: public; Owner: admin
--

CREATE INDEX NONCONCURRENTLY ix_shipments_backendreference_1 ON public.dispatches_1 USING lsm (backendreference HASH) SPLIT INTO 6 TABLETS WHERE (backendreference IS NOT NULL);



--
-- Name: ix_shipments_hasdeviations_1; Type: INDEX; Schema: public; Owner: admin
--

CREATE INDEX NONCONCURRENTLY ix_shipments_hasdeviations_1 ON public.dispatches_1 USING lsm (hasdeviations ASC) WHERE (hasdeviations = true);


--
-- Name: ix_shipments_id_hash_1; Type: INDEX; Schema: public; Owner: admin; Tablespace: region_1
--

CREATE INDEX NONCONCURRENTLY ix_shipments_id_hash_1 ON public.dispatches_1 USING lsm (((yb_hash_code(id) % 6)) ASC, id DESC);


--
-- Name: ix_shipments_iscompleted_1; Type: INDEX; Schema: public; Owner: admin
--

CREATE INDEX NONCONCURRENTLY ix_shipments_iscompleted_1 ON public.dispatches_1 USING lsm (iscompleted ASC) WHERE (iscompleted IS NOT NULL);



--
-- Name: ix_shipments_ordertypeid_1; Type: INDEX; Schema: public; Owner: admin
--

CREATE INDEX NONCONCURRENTLY ix_shipments_ordertypeid_1 ON public.dispatches_1 USING lsm (ordertypeid ASC) WHERE (ordertypeid IS NOT NULL);


--
-- Name: ix_shipments_organizationunitid_1; Type: INDEX; Schema: public; Owner: admin; Tablespace: region_1
--

CREATE INDEX NONCONCURRENTLY ix_shipments_organizationunitid_1 ON public.dispatches_1 USING lsm (((yb_hash_code(id) % 6)) ASC, organizationunitid DESC, id DESC);


--
-- Name: ix_shipments_receivemethodid_1; Type: INDEX; Schema: public; Owner: admin
--

CREATE INDEX NONCONCURRENTLY ix_shipments_receivemethodid_1 ON public.dispatches_1 USING lsm (receivemethodid ASC) WHERE (receivemethodid IS NOT NULL);



--
-- Name: ix_userid; Type: INDEX; Schema: public; Owner: admin
--

CREATE INDEX NONCONCURRENTLY ix_userid ON public.savedlistlinks USING lsm (userid ASC);



--
-- Name: ix_users_authenticationid_1; Type: INDEX; Schema: public; Owner: admin
--

CREATE INDEX NONCONCURRENTLY ix_users_authenticationid_1 ON public.accounts_1 USING lsm (authenticationid HASH) SPLIT INTO 6 TABLETS WHERE (authenticationid IS NOT NULL);



--
-- Name: ix_users_backendid_1; Type: INDEX; Schema: public; Owner: admin
--

CREATE INDEX NONCONCURRENTLY ix_users_backendid_1 ON public.accounts_1 USING lsm (backendid HASH) SPLIT INTO 6 TABLETS WHERE (backendid IS NOT NULL);



--
-- Name: ix_users_backendrelationid_1; Type: INDEX; Schema: public; Owner: admin
--

CREATE INDEX NONCONCURRENTLY ix_users_backendrelationid_1 ON public.accounts_1 USING lsm (backendrelationid HASH) SPLIT INTO 6 TABLETS WHERE (backendrelationid IS NOT NULL);



--
-- Name: ix_users_creationtime_1; Type: INDEX; Schema: public; Owner: admin
--

CREATE INDEX NONCONCURRENTLY ix_users_creationtime_1 ON public.accounts_1 USING lsm (creationtime ASC);



--
-- Name: ix_users_customid_1; Type: INDEX; Schema: public; Owner: admin
--

CREATE INDEX NONCONCURRENTLY ix_users_customid_1 ON public.accounts_1 USING lsm (customid HASH) SPLIT INTO 6 TABLETS WHERE (customid IS NOT NULL);



--
-- Name: ix_users_emailaddress_1; Type: INDEX; Schema: public; Owner: admin
--

CREATE INDEX NONCONCURRENTLY ix_users_emailaddress_1 ON public.accounts_1 USING lsm (lower((emailaddress)::text) HASH) SPLIT INTO 6 TABLETS WHERE (emailaddress IS NOT NULL);


--
-- Name: ix_users_id_hash_1; Type: INDEX; Schema: public; Owner: admin; Tablespace: region_1
--

CREATE INDEX NONCONCURRENTLY ix_users_id_hash_1 ON public.accounts_1 USING lsm (((yb_hash_code(id) % 6)) ASC, id DESC);


--
-- Name: ix_users_isemployee_1; Type: INDEX; Schema: public; Owner: admin
--

CREATE INDEX NONCONCURRENTLY ix_users_isemployee_1 ON public.accounts_1 USING lsm (isemployee ASC) INCLUDE (isdeleted);



--
-- Name: ix_users_singlesignonid_1; Type: INDEX; Schema: public; Owner: admin
--

CREATE INDEX NONCONCURRENTLY ix_users_singlesignonid_1 ON public.accounts_1 USING lsm (singlesignonid HASH) SPLIT INTO 6 TABLETS WHERE (singlesignonid IS NOT NULL);



--
-- Name: ix_users_socialsecuritynumber_1; Type: INDEX; Schema: public; Owner: admin
--

CREATE INDEX NONCONCURRENTLY ix_users_socialsecuritynumber_1 ON public.accounts_1 USING lsm (socialsecuritynumber HASH) SPLIT INTO 6 TABLETS WHERE (socialsecuritynumber IS NOT NULL);


--
-- Name: ix_users_typeid_anonymous_1; Type: INDEX; Schema: public; Owner: admin; Tablespace: region_1
--

CREATE INDEX NONCONCURRENTLY ix_users_typeid_anonymous_1 ON public.accounts_1 USING lsm (typeid HASH) SPLIT INTO 6 TABLETS WHERE ((typeid = 4) AND (isdeleted = false));



--
-- Name: ix_usertasks_financialperiodid_1; Type: INDEX; Schema: public; Owner: admin
--

CREATE INDEX NONCONCURRENTLY ix_usertasks_financialperiodid_1 ON public.accounttasks_1 USING lsm (financialperiodid HASH) SPLIT INTO 6 TABLETS WHERE (financialperiodid IS NOT NULL);


--
-- Name: ix_usertasks_id_hash_1; Type: INDEX; Schema: public; Owner: admin; Tablespace: region_1
--

CREATE INDEX NONCONCURRENTLY ix_usertasks_id_hash_1 ON public.accounttasks_1 USING lsm (((yb_hash_code(id) % 6)) ASC, id DESC);


--
-- Name: ix_usertasks_isactive_iscompleted_typeid_organizationunitid_1; Type: INDEX; Schema: public; Owner: admin
--

CREATE INDEX NONCONCURRENTLY ix_usertasks_isactive_iscompleted_typeid_organizationunitid_1 ON public.accounttasks_1 USING lsm (isactive ASC, iscompleted ASC, typeid ASC, organizationunitid ASC) WHERE (isactive = true);


--
-- Name: ix_usertasks_organiza_iscomple_isactive_typeid_userid_1; Type: INDEX; Schema: public; Owner: admin; Tablespace: region_1
--

CREATE INDEX NONCONCURRENTLY ix_usertasks_organiza_iscomple_isactive_typeid_userid_1 ON public.accounttasks_1 USING lsm (((yb_hash_code(id) % 6)) ASC, organizationunitid DESC, iscompleted DESC, isactive DESC, typeid DESC, userid DESC, id DESC) INCLUDE (creationtime, priority);



--
-- Name: ix_usertasks_organizationunitid_1; Type: INDEX; Schema: public; Owner: admin; Tablespace: region_1
--

CREATE INDEX NONCONCURRENTLY ix_usertasks_organizationunitid_1 ON public.accounttasks_1 USING lsm (((yb_hash_code(id) % 6)) ASC, organizationunitid DESC, id DESC) INCLUDE (isactive, iscompleted, lastmodificationtime);



--
-- Name: ix_usertasks_organizationunitid_iscompleted_isactive_1; Type: INDEX; Schema: public; Owner: admin; Tablespace: region_1
--

CREATE INDEX NONCONCURRENTLY ix_usertasks_organizationunitid_iscompleted_isactive_1 ON public.accounttasks_1 USING lsm (((yb_hash_code(id) % 6)) ASC, organizationunitid DESC, iscompleted DESC, isactive DESC, id DESC) INCLUDE (typeid, subtypeid);


--
-- Name: ix_usertasks_parentid_1; Type: INDEX; Schema: public; Owner: admin
--

CREATE INDEX NONCONCURRENTLY ix_usertasks_parentid_1 ON public.accounttasks_1 USING lsm (parentid HASH) SPLIT INTO 6 TABLETS WHERE (parentid IS NOT NULL);


--
-- Name: ix_usertasks_userid_1; Type: INDEX; Schema: public; Owner: admin; Tablespace: region_1
--

CREATE INDEX NONCONCURRENTLY ix_usertasks_userid_1 ON public.accounttasks_1 USING lsm (((yb_hash_code(id) % 6)) ASC, userid DESC, id DESC) WHERE (userid IS NOT NULL);



--
-- Name: ix_usertasks_userid_typeid_iscompl_isactiv_deadlin_startti_1; Type: INDEX; Schema: public; Owner: admin; Tablespace: region_1
--

CREATE INDEX NONCONCURRENTLY ix_usertasks_userid_typeid_iscompl_isactiv_deadlin_startti_1 ON public.accounttasks_1 USING lsm (((yb_hash_code(id) % 6)) ASC, userid DESC, typeid DESC, iscompleted DESC, isactive DESC, deadline DESC, starttime DESC, id DESC) WHERE (userid IS NOT NULL);



--
-- Name: ix_wishlistdata_searchkey_1; Type: INDEX; Schema: public; Owner: admin
--

CREATE INDEX NONCONCURRENTLY ix_wishlistdata_searchkey_1 ON public.savedlistdata_1 USING lsm (searchkey HASH) SPLIT INTO 6 TABLETS WHERE (searchkey IS NOT NULL);



--
-- Name: ix_wishlistdata_wishlistid_type_1; Type: INDEX; Schema: public; Owner: admin
--

CREATE INDEX NONCONCURRENTLY ix_wishlistdata_wishlistid_type_1 ON public.savedlistdata_1 USING lsm (wishlistid HASH, type ASC) SPLIT INTO 6 TABLETS WHERE (wishlistid IS NOT NULL);



--
-- Name: ix_wishlistid; Type: INDEX; Schema: public; Owner: admin
--

CREATE INDEX NONCONCURRENTLY ix_wishlistid ON public.savedlistshares USING lsm (wishlistid ASC);



--
-- Name: ix_wishlistproducts_wishlistid_1; Type: INDEX; Schema: public; Owner: admin
--

CREATE INDEX NONCONCURRENTLY ix_wishlistproducts_wishlistid_1 ON public.savedlistitems_1 USING lsm (wishlistid HASH) SPLIT INTO 6 TABLETS;


--
-- Name: ix_wishlists_id_hash_1; Type: INDEX; Schema: public; Owner: admin; Tablespace: region_1
--

CREATE INDEX NONCONCURRENTLY ix_wishlists_id_hash_1 ON public.savedlists_1 USING lsm (((yb_hash_code(id) % 6)) ASC, id DESC);


--
-- Name: ix_wishlists_userid_1; Type: INDEX; Schema: public; Owner: admin
--

CREATE INDEX NONCONCURRENTLY ix_wishlists_userid_1 ON public.savedlists_1 USING lsm (userid HASH) SPLIT INTO 6 TABLETS WHERE (userid IS NOT NULL);



--
-- Name: ux_apikeys_userid_authenticationtokenhash; Type: INDEX; Schema: public; Owner: admin
--

CREATE UNIQUE INDEX NONCONCURRENTLY ux_apikeys_userid_authenticationtokenhash ON public.accesskeys USING lsm (userid ASC, authenticationtokenhash ASC);



--
-- Name: ux_auditchainlocks_1id; Type: INDEX; Schema: public; Owner: admin
--

CREATE UNIQUE INDEX NONCONCURRENTLY ux_auditchainlocks_1id ON public.eventchainlocks_1 USING lsm (id HASH, typeid ASC) SPLIT INTO 6 TABLETS;



--
-- Name: ux_auditchains_1; Type: INDEX; Schema: public; Owner: admin
--

CREATE UNIQUE INDEX NONCONCURRENTLY ux_auditchains_1 ON public.eventchains_1 USING lsm (identifier HASH, discriminator ASC, typeid ASC, version ASC, organizationunitid ASC, isoffline ASC) SPLIT INTO 6 TABLETS;



--
-- Name: ux_discountdetails_id_1; Type: INDEX; Schema: public; Owner: admin
--

CREATE UNIQUE INDEX NONCONCURRENTLY ux_discountdetails_id_1 ON public.pricediscounts_1 USING lsm (id HASH) SPLIT INTO 6 TABLETS;



--
-- Name: ux_invoicelines_id_1; Type: INDEX; Schema: public; Owner: admin
--

CREATE UNIQUE INDEX NONCONCURRENTLY ux_invoicelines_id_1 ON public.billinglines_1 USING lsm (id HASH) SPLIT INTO 6 TABLETS;



--
-- Name: ux_invoicemanualdata_id_1; Type: INDEX; Schema: public; Owner: admin
--

CREATE UNIQUE INDEX NONCONCURRENTLY ux_invoicemanualdata_id_1 ON public.billingmanualdata_1 USING lsm (id HASH) SPLIT INTO 6 TABLETS;



--
-- Name: ux_invoicepayments_id_1; Type: INDEX; Schema: public; Owner: admin
--

CREATE UNIQUE INDEX NONCONCURRENTLY ux_invoicepayments_id_1 ON public.billingpayments_1 USING lsm (id HASH) SPLIT INTO 6 TABLETS;



--
-- Name: ux_invoices_backendsystemid_backendid_1; Type: INDEX; Schema: public; Owner: admin
--

CREATE UNIQUE INDEX NONCONCURRENTLY ux_invoices_backendsystemid_backendid_1 ON public.billings_1 USING lsm (backendsystemid HASH, backendid ASC) SPLIT INTO 6 TABLETS WHERE ((backendsystemid IS NOT NULL) AND (backendid IS NOT NULL));



--
-- Name: ux_orderledger_id_1; Type: INDEX; Schema: public; Owner: admin
--

CREATE UNIQUE INDEX NONCONCURRENTLY ux_orderledger_id_1 ON public.salesledger_1 USING lsm (id HASH) SPLIT INTO 6 TABLETS;



--
-- Name: ux_orderlines_id_1; Type: INDEX; Schema: public; Owner: admin
--

CREATE UNIQUE INDEX NONCONCURRENTLY ux_orderlines_id_1 ON public.saleslines_1 USING lsm (id HASH) SPLIT INTO 6 TABLETS;



--
-- Name: ux_orders_clientorderid_1; Type: INDEX; Schema: public; Owner: admin
--

CREATE UNIQUE INDEX NONCONCURRENTLY ux_orders_clientorderid_1 ON public.sales_1 USING lsm (clientorderid HASH) SPLIT INTO 6 TABLETS WHERE (clientorderid IS NOT NULL);



--
-- Name: ux_organizationunits_globalid; Type: INDEX; Schema: public; Owner: admin
--

CREATE UNIQUE INDEX NONCONCURRENTLY ux_organizationunits_globalid ON public.branches USING lsm (globalid ASC) WHERE (globalid IS NOT NULL);



--
-- Name: ux_paymenttransactions_id_1; Type: INDEX; Schema: public; Owner: admin
--

CREATE UNIQUE INDEX NONCONCURRENTLY ux_paymenttransactions_id_1 ON public.paymentevents_1 USING lsm (id HASH) SPLIT INTO 6 TABLETS;



--
-- Name: ux_shipmentlines_id_1; Type: INDEX; Schema: public; Owner: admin
--

CREATE UNIQUE INDEX NONCONCURRENTLY ux_shipmentlines_id_1 ON public.dispatchlines_1 USING lsm (id HASH) SPLIT INTO 6 TABLETS;



--
-- Name: ux_users_backendsystemid_backendid_1; Type: INDEX; Schema: public; Owner: admin
--

CREATE UNIQUE INDEX NONCONCURRENTLY ux_users_backendsystemid_backendid_1 ON public.accounts_1 USING lsm (backendsystemid HASH, backendid ASC) SPLIT INTO 6 TABLETS WHERE ((backendsystemid IS NOT NULL) AND (backendid IS NOT NULL) AND (isdeleted = false) AND (isincognito = false) AND (accounttypeid <> 2));



--
-- Name: ux_users_emailaddress_discriminator_visibilitygroupid_1; Type: INDEX; Schema: public; Owner: admin
--

CREATE UNIQUE INDEX NONCONCURRENTLY ux_users_emailaddress_discriminator_visibilitygroupid_1 ON public.accounts_1 USING lsm (lower((emailaddress)::text) HASH, discriminator ASC, visibilitygroupid ASC) SPLIT INTO 6 TABLETS WHERE ((isdeleted = false) AND (accounttypeid = 0) AND (isincognito = false) AND (emailaddress IS NOT NULL));



--
-- Name: ux_users_globalid_1; Type: INDEX; Schema: public; Owner: admin
--

CREATE UNIQUE INDEX NONCONCURRENTLY ux_users_globalid_1 ON public.accounts_1 USING lsm (globalid HASH) SPLIT INTO 6 TABLETS WHERE (globalid IS NOT NULL);



--
-- Name: ux_users_nickname_discriminator_visibilitygroupid_1; Type: INDEX; Schema: public; Owner: admin
--

CREATE UNIQUE INDEX NONCONCURRENTLY ux_users_nickname_discriminator_visibilitygroupid_1 ON public.accounts_1 USING lsm (lower((nickname)::text) HASH, discriminator ASC, visibilitygroupid ASC) SPLIT INTO 6 TABLETS WHERE ((isdeleted = false) AND (accounttypeid = 0) AND (isincognito = false) AND (nickname IS NOT NULL));

SET default_tablespace = '';
