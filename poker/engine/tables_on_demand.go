//go:build tables_on_demand || (wasip1 && !tables_in_memory)

package engine

const tablesOnDemand = true // see readTablesOnDemand
