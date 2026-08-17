output "dashboard_guid" {
  description = "GUID of the LiveCart New Relic dashboard (Macro + Micro pages)."
  value       = newrelic_one_dashboard.live_commerce.guid
}

output "dashboard_permalink" {
  description = "URL to open the LiveCart New Relic dashboard in the browser."
  value       = newrelic_one_dashboard.live_commerce.permalink
}
