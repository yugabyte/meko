-- Quick post-seed validation for happy-path load test readiness (cutdown schema).

SELECT 'sales' AS table_name, COUNT(*)::bigint AS rows FROM public.sales WHERE regionid = 1
UNION ALL
SELECT 'saleslines', COUNT(*) FROM public.saleslines WHERE regionid = 1
UNION ALL
SELECT 'pricediscounts', COUNT(*) FROM public.pricediscounts WHERE regionid = 1
UNION ALL
SELECT 'paymentevents', COUNT(*) FROM public.paymentevents WHERE regionid = 1
UNION ALL
SELECT 'billings', COUNT(*) FROM public.billings WHERE regionid = 1
UNION ALL
SELECT 'billinglines', COUNT(*) FROM public.billinglines WHERE regionid = 1
UNION ALL
SELECT 'billingpayments', COUNT(*) FROM public.billingpayments WHERE regionid = 1
UNION ALL
SELECT 'dispatches', COUNT(*) FROM public.dispatches WHERE regionid = 1
UNION ALL
SELECT 'dispatchlines', COUNT(*) FROM public.dispatchlines WHERE regionid = 1
UNION ALL
SELECT 'backgroundjobs', COUNT(*) FROM public.backgroundjobs WHERE regionid = 1
UNION ALL
SELECT 'savedlists', COUNT(*) FROM public.savedlists WHERE regionid = 1
UNION ALL
SELECT 'savedlistitems', COUNT(*) FROM public.savedlistitems WHERE regionid = 1
UNION ALL
SELECT 'savedlistdata', COUNT(*) FROM public.savedlistdata WHERE regionid = 1
UNION ALL
SELECT 'eventchainlocks_1', COUNT(*) FROM public.eventchainlocks_1 WHERE regionid = 1
UNION ALL
SELECT 'eventchains_1', COUNT(*) FROM public.eventchains_1 WHERE regionid = 1
UNION ALL
SELECT 'eventchainledger_1', COUNT(*) FROM public.eventchainledger_1 WHERE regionid = 1
UNION ALL
SELECT 'itemledger_1', COUNT(*) FROM public.itemledger_1 WHERE regionid = 1
UNION ALL
SELECT 'inventory_1', COUNT(*) FROM public.inventory_1 WHERE regionid = 1
UNION ALL
SELECT 'accounts', COUNT(*) FROM public.accounts WHERE regionid = 1
UNION ALL
SELECT 'accesskeys', COUNT(*) FROM public.accesskeys
UNION ALL
SELECT 'accountingperiods', COUNT(*) FROM public.accountingperiods WHERE regionid = 1
UNION ALL
SELECT 'accounttasks', COUNT(*) FROM public.accounttasks WHERE regionid = 1
ORDER BY table_name;
