-- Happy-path seed for workload replay.
-- Assumptions: RegionID = 1, UUIDs generated in UUIDv7-compatible shape.

CREATE SCHEMA IF NOT EXISTS lt;

CREATE OR REPLACE FUNCTION lt.uuidv7()
RETURNS uuid
LANGUAGE plpgsql
AS $$
DECLARE
  unix_ms bigint;
  ts_hex text;
  rand_hex text;
  part1 text;
  part2 text;
  part3 text;
  part4 text;
  part5 text;
  variant_nibble text;
BEGIN
  unix_ms := floor(extract(epoch FROM clock_timestamp()) * 1000)::bigint;
  ts_hex := lpad(to_hex(unix_ms), 12, '0');
  rand_hex := md5(random()::text || clock_timestamp()::text || txid_current()::text);

  part1 := substr(ts_hex, 1, 8);
  part2 := substr(ts_hex, 9, 4);
  part3 := '7' || substr(rand_hex, 1, 3);

  variant_nibble := substr('89ab', (floor(random() * 4)::int) + 1, 1);
  part4 := variant_nibble || substr(rand_hex, 4, 3);
  part5 := substr(rand_hex, 8, 12);

  RETURN (part1 || '-' || part2 || '-' || part3 || '-' || part4 || '-' || part5)::uuid;
END;
$$;

DO $$
DECLARE
  v_region smallint := 1;
  v_actor uuid;
  v_company uuid;
  v_address uuid;
  v_orgunit uuid;
  v_roleset uuid;
  v_taxcode uuid;
  v_stocklabel uuid;
  v_payment_type uuid;

  v_order uuid;
  v_orderline_1 uuid;
  v_orderline_2 uuid;
  v_payment_tx uuid;
  v_invoice uuid;
  v_invoice_userdata uuid;
  v_shipment uuid;
  v_wishlist uuid;
  v_wsr uuid;
  v_user uuid;
  v_fp_prev uuid;
  v_chain uuid;
  v_product uuid;

  i integer;
BEGIN

  v_actor := lt.uuidv7();
  v_company := lt.uuidv7();
  v_address := lt.uuidv7();
  v_orgunit := lt.uuidv7();
  v_roleset := lt.uuidv7();
  v_taxcode := lt.uuidv7();
  v_stocklabel := lt.uuidv7();
  v_payment_type := lt.uuidv7();

  INSERT INTO public.locations (
    id, regionid, countryid, createdbyid, creationtime,
    city, street, housenumber, zipcode, address1
  ) VALUES (
    v_address, v_region, 'NLD', v_actor, clock_timestamp(),
    'Amsterdam', 'Loadteststraat', '1', '1011AB', 'Loadteststraat 1'
  ) ON CONFLICT DO NOTHING;

  INSERT INTO public.businesses (
    id, regionid, createdbyid, creationtime, name, typeid, isdeleted
  ) VALUES (
    v_company, v_region, v_actor, clock_timestamp(), 'LT Company', 0, false
  ) ON CONFLICT DO NOTHING;

  INSERT INTO public.branches (
    id, name, typeid, createdbyid, creationtime, countryid, currencyid,
    costpricecurrencyid, rolesetid, isdeleted, companyid
  ) VALUES (
    v_orgunit, 'LT Main OU', 1, v_actor, clock_timestamp(),
    'NLD', 'EUR', 'EUR', v_roleset, false, v_company
  ) ON CONFLICT DO NOTHING;

  -- accounts for order/customer and wishlist/query joins.
  FOR i IN 1..80 LOOP
    v_user := lt.uuidv7();
    INSERT INTO public.accounts (
      id, regionid, languageid, typeid, createdbyid, creationtime,
      isdeleted, companyid, shippingaddressid, billingaddressid,
      countryid, sessionkey
    ) VALUES (
      v_user, v_region, 'en', 1, v_actor, clock_timestamp(),
      false, v_company, v_address, v_address,
      'NLD', substr(md5(v_user::text), 1, 6)
    ) ON CONFLICT DO NOTHING;
  END LOOP;

  -- Scheduled tasks for Q02/Q10.
  FOR i IN 1..50 LOOP
    INSERT INTO public.backgroundjobs (
      id, name, isrecurring, creationtime, regionid,
      nextexecutiontime, executioncount, successcount
    ) VALUES (
      format('lt-task-%s', i),
      format('LT Task %s', i),
      true,
      clock_timestamp() - ((i % 5) || ' minutes')::interval,
      v_region,
      clock_timestamp() + ((i % 5) || ' minutes')::interval,
      i,
      i
    ) ON CONFLICT DO NOTHING;
  END LOOP;

  -- Financial periods for Q26.
  v_fp_prev := NULL;
  FOR i IN 1..30 LOOP
    v_chain := lt.uuidv7();
    INSERT INTO public.accountingperiods (
      id, regionid, previousperiodid, statusid, organizationunitid,
      createdbyid, creationtime, currencyid, costpricecurrencyid,
      openingtime, closingtime
    ) VALUES (
      v_chain, v_region, v_fp_prev, (i % 3), v_orgunit,
      v_actor, clock_timestamp() - ((31 - i) || ' days')::interval,
      'EUR', 'EUR',
      clock_timestamp() - ((31 - i) || ' days')::interval,
      clock_timestamp() - ((30 - i) || ' days')::interval
    ) ON CONFLICT DO NOTHING;

    v_fp_prev := v_chain;
  END LOOP;

  -- API keys for Q24.
  FOR i IN 1..60 LOOP
    SELECT id INTO v_user
    FROM public.accounts
    WHERE regionid = v_region AND isdeleted = false
    ORDER BY random()
    LIMIT 1;

    INSERT INTO public.accesskeys (
      id, userid, authenticationtokenhash, isactive, expirationdate,
      createdbyid, creationtime, description
    ) VALUES (
      lt.uuidv7(), v_user, md5('lt-api-' || i::text), true,
      clock_timestamp() + interval '30 days',
      v_actor, clock_timestamp(), format('lt-api-%s', i)
    ) ON CONFLICT DO NOTHING;
  END LOOP;

  -- User tasks for Q23.
  FOR i IN 1..40 LOOP
    INSERT INTO public.accounttasks (
      id, regionid, userid, typeid, priority, iscompleted, isactive,
      createdbyid, creationtime, organizationunitid, starttime, data
    ) VALUES (
      lt.uuidv7(), v_region, NULL, '00000000-0000-0000-0000-0000000000aa'::uuid,
      1, false, true,
      v_actor, clock_timestamp(), v_orgunit, NULL,
      format('{"task":%s}', i)
    ) ON CONFLICT DO NOTHING;
  END LOOP;

  -- Wishlist family for Q12/Q19/Q20/Q22.
  FOR i IN 1..200 LOOP
    SELECT id INTO v_user
    FROM public.accounts
    WHERE regionid = v_region AND isdeleted = false
    ORDER BY random()
    LIMIT 1;

    v_wishlist := lt.uuidv7();

    INSERT INTO public.savedlists (
      id, regionid, createdbyid, creationtime, userid, name, isdeleted, isshared
    ) VALUES (
      v_wishlist, v_region, v_actor, clock_timestamp(), v_user,
      format('LT Wishlist %s', i), false, (i % 4 = 0)
    ) ON CONFLICT DO NOTHING;

    INSERT INTO public.savedlistitems (
      id, regionid, createdbyid, creationtime, wishlistid, productid
    ) VALUES (
      lt.uuidv7(), v_region, v_actor, clock_timestamp(), v_wishlist, lt.uuidv7()
    ) ON CONFLICT DO NOTHING;

    INSERT INTO public.savedlistdata (
      id, regionid, wishlistid, type, searchkey, data, createdbyid, creationtime
    ) VALUES (
      lt.uuidv7(), v_region, v_wishlist, 'search',
      format('k-%s', i), format('{"i":%s}', i), v_actor, clock_timestamp()
    ) ON CONFLICT DO NOTHING;

    IF (i % 4 = 0) THEN
      v_wsr := lt.uuidv7();

      INSERT INTO public.savedlistshares (
        id, wishlistid, description, sharedon, token, privilegesid, isdeleted
      ) VALUES (
        v_wsr, v_wishlist, 'lt-share', clock_timestamp(),
        substr(md5(v_wishlist::text), 1, 32), 1, false
      ) ON CONFLICT DO NOTHING;

      INSERT INTO public.savedlistlinks (
        userid, wishlistshareregistrationid, wishlistid, linkedon
      ) VALUES (
        v_user, v_wsr, v_wishlist, clock_timestamp()
      ) ON CONFLICT DO NOTHING;
    END IF;
  END LOOP;

  -- Audit chain lock rows for Q11.
  FOR i IN 1..200 LOOP
    INSERT INTO public.eventchainlocks_1 (id, typeid, regionid)
    VALUES (i, 1 + (i % 3), v_region)
    ON CONFLICT DO NOTHING;
  END LOOP;

  -- Baseline inventory/item ledger.
  FOR i IN 1..200 LOOP
    v_product := lt.uuidv7();

    INSERT INTO public.inventory_1 (
      organizationunitid, productid, stocklabelid, regionid,
      quantityonhand, quantityallocated
    ) VALUES (
      v_orgunit, v_product, v_stocklabel, v_region,
      100 + (i % 25), 0
    ) ON CONFLICT DO NOTHING;

    INSERT INTO public.itemledger_1 (
      id, regionid, typeid, productid, organizationunitid,
      quantity, quantityallocated, remark, stocklabelid, issummary,
      stockmutationid, orderlineid, createdbyid, creationtime
    ) VALUES (
      lt.uuidv7(), v_region, 1, v_product, v_orgunit,
      1, 0, 'lt-seed', v_stocklabel, false,
      lt.uuidv7(), NULL, v_actor, clock_timestamp()
    ) ON CONFLICT DO NOTHING;
  END LOOP;

  -- Core commercial graph for Q03/Q04/Q05/Q06/Q07/Q08/Q13/Q16 + tx path.
  FOR i IN 1..400 LOOP
    SELECT id INTO v_user
    FROM public.accounts
    WHERE regionid = v_region AND isdeleted = false
    ORDER BY random()
    LIMIT 1;

    v_order := lt.uuidv7();
    v_orderline_1 := lt.uuidv7();
    v_orderline_2 := lt.uuidv7();
    v_payment_tx := lt.uuidv7();
    v_invoice := lt.uuidv7();
    v_invoice_userdata := lt.uuidv7();
    v_shipment := lt.uuidv7();

    INSERT INTO public.sales (
      id, regionid, customerid, shippingaddressid, billingaddressid, pickupaddressid,
      createdbyid, creationtime,
      originatingorganizationunitid, currencyid, organizationunitid,
      pickuporganizationunitid, soldfromorganizationunitid,
      description, iscompleted, ispaid, isinvoiced, isshipped
    ) VALUES (
      v_order, v_region, v_user, v_address, v_address, v_address,
      v_actor, clock_timestamp(),
      v_orgunit, 'EUR', v_orgunit,
      v_orgunit, v_orgunit,
      'lt-order', false, true, true, true
    ) ON CONFLICT DO NOTHING;

    INSERT INTO public.saleslines (
      id, regionid, orderid, productid, typeid, description,
      quantityordered, unitprice, createdbyid, creationtime,
      stocklabelid, lineactiontypeid, currencyid, exchangerate,
      taxcodeid, costpricecurrencyid, organizationunitid,
      quantityopen, quantitytoship, quantitytoreserve
    ) VALUES
    (
      v_orderline_1, v_region, v_order, lt.uuidv7(), 1, 'lt-line-1',
      1, 10.00, v_actor, clock_timestamp(),
      v_stocklabel, 1, 'EUR', 1.0,
      v_taxcode, 'EUR', v_orgunit,
      1, 1, 1
    ),
    (
      v_orderline_2, v_region, v_order, lt.uuidv7(), 1, 'lt-line-2',
      2, 12.50, v_actor, clock_timestamp(),
      v_stocklabel, 1, 'EUR', 1.0,
      v_taxcode, 'EUR', v_orgunit,
      2, 2, 2
    )
    ON CONFLICT DO NOTHING;

    INSERT INTO public.paymentevents (
      id, regionid, orderid, amount, paidamount,
      createdbyid, creationtime, typeid, change,
      organizationunitid, currencyid, description, statusid
    ) VALUES (
      v_payment_tx, v_region, v_order, 22.50, 22.50,
      v_actor, clock_timestamp(), v_payment_type, 0,
      v_orgunit, 'EUR', 'lt-payment', 1
    ) ON CONFLICT DO NOTHING;

    INSERT INTO public.pricediscounts (
      id, regionid, parentorderlineid, discountorderlineid,
      discountid, discountamount, orderid
    ) VALUES (
      lt.uuidv7(), v_region, v_orderline_1, v_orderline_2,
      lt.uuidv7(), 1.25, v_order
    ) ON CONFLICT DO NOTHING;

    INSERT INTO public.dispatches (
      id, regionid, createdbyid, creationtime,
      organizationunitid, optionsid, statusid, isexported, isdeleted
    ) VALUES (
      v_shipment, v_region, v_actor, clock_timestamp(),
      v_orgunit, 0, 1, false, false
    ) ON CONFLICT DO NOTHING;

    INSERT INTO public.dispatchlines (
      id, regionid, createdbyid, creationtime,
      shipmentid, orderlineid, quantityshipped, orderid, isdeleted
    ) VALUES (
      lt.uuidv7(), v_region, v_actor, clock_timestamp(),
      v_shipment, v_orderline_1, 1, v_order, false
    ) ON CONFLICT DO NOTHING;

    INSERT INTO public.billinguserdata (
      id, regionid, createdbyid, creationtime,
      companyid, billingaddressid, shippingaddressid,
      organizationunitcompanyid, organizationunitaddressid,
      emailaddress, firstname, lastname
    ) VALUES (
      v_invoice_userdata, v_region, v_actor, clock_timestamp(),
      v_company, v_address, v_address,
      v_company, v_address,
      'loadtest.invalid', 'Load', 'Test'
    ) ON CONFLICT DO NOTHING;

    INSERT INTO public.billings (
      id, regionid, createdbyid, creationtime,
      organizationunitid, typeid, statusid,
      userdataid, currencyid, invoicenumber, invoicedate,
      totalamount, chainid, sequencenumber, signature
    ) VALUES (
      v_invoice, v_region, v_actor, clock_timestamp(),
      v_orgunit, 1, 1,
      v_invoice_userdata, 'EUR', format('INV-%s', i), clock_timestamp(),
      22.50, lt.uuidv7(), i, substr(md5(v_invoice::text), 1, 16)
    ) ON CONFLICT DO NOTHING;

    INSERT INTO public.billingmanualdata (
      id, regionid, createdbyid, creationtime,
      invoiceid, series, number
    ) VALUES (
      lt.uuidv7(), v_region, v_actor, clock_timestamp(),
      v_invoice, 'A', i::text
    ) ON CONFLICT DO NOTHING;

    INSERT INTO public.billinglines (
      id, regionid, createdbyid, creationtime,
      invoiceid, orderlineid, quantityinvoiced,
      discountamount, invoicedbyid, quantitytoinvoice,
      shipmentlineid, orderid
    )
    SELECT
      lt.uuidv7(), v_region, v_actor, clock_timestamp(),
      v_invoice, v_orderline_1, 1,
      0, v_actor, 1,
      sl.id, v_order
    FROM public.dispatchlines sl
    WHERE sl.regionid = v_region
      AND sl.shipmentid = v_shipment
      AND sl.orderlineid = v_orderline_1
    LIMIT 1
    ON CONFLICT DO NOTHING;

    INSERT INTO public.billingpayments (
      id, regionid, createdbyid, creationtime,
      invoiceid, paymenttransactionid, amount
    ) VALUES (
      lt.uuidv7(), v_region, v_actor, clock_timestamp(),
      v_invoice, v_payment_tx, 22.50
    ) ON CONFLICT DO NOTHING;

    INSERT INTO public.salesledger (
      id, regionid, orderlineid, typeid, description,
      createdbyid, creationtime, orderid, oldvalue, newvalue
    ) VALUES (
      lt.uuidv7(), v_region, v_orderline_1, lt.uuidv7(),
      'lt-seed-ledger', v_actor, clock_timestamp(), v_order,
      NULL, NULL
    ) ON CONFLICT DO NOTHING;

    -- Seed baseline audit chain state for the order.
    INSERT INTO public.eventchains_1 (
      id, regionid, identifier, discriminator, typeid, version,
      createdbyid, creationtime, state
    ) VALUES (
      lt.uuidv7(), v_region, v_order::text, 1, 1, 1,
      v_actor, clock_timestamp(),
      jsonb_build_object('seq', 1, 'sig', substr(md5(v_order::text), 1, 16))
    )
    ON CONFLICT DO NOTHING;
  END LOOP;

  -- Baseline ledger rows for existing chains.
  FOR i IN 1..150 LOOP
    SELECT id INTO v_chain
    FROM public.eventchains_1
    WHERE regionid = v_region
    ORDER BY random()
    LIMIT 1;

    IF v_chain IS NOT NULL THEN
      INSERT INTO public.eventchainledger_1 (
        id, regionid, chainid, newsequencenumber, newsignature,
        createdbyid, creationtime
      ) VALUES (
        lt.uuidv7(), v_region, v_chain, i,
        substr(md5((v_chain::text || i::text)), 1, 16),
        v_actor, clock_timestamp()
      ) ON CONFLICT DO NOTHING;
    END IF;
  END LOOP;

  RAISE NOTICE 'Seed happy_path_v1 applied.';
END;
$$;

ANALYZE public.sales;
ANALYZE public.saleslines;
ANALYZE public.pricediscounts;
ANALYZE public.paymentevents;
ANALYZE public.billings;
ANALYZE public.billinglines;
ANALYZE public.billingpayments;
ANALYZE public.dispatches;
ANALYZE public.dispatchlines;
ANALYZE public.savedlists;
ANALYZE public.savedlistitems;
ANALYZE public.savedlistdata;
ANALYZE public.backgroundjobs;
ANALYZE public.eventchains_1;
ANALYZE public.eventchainledger_1;
ANALYZE public.itemledger_1;
ANALYZE public.inventory_1;
ANALYZE public.accounttasks;
ANALYZE public.accesskeys;
ANALYZE public.accountingperiods;
