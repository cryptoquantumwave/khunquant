import { useQuery } from "@tanstack/react-query"
import { getSandboxStatus, getFixtures } from "@/api/sandbox"

export function useSandboxStatus() {
  return useQuery({
    queryKey: ["sandbox", "status"],
    queryFn: getSandboxStatus,
    staleTime: 10000, // 10 seconds
  })
}

export function useFixtures(venue: string | null) {
  return useQuery({
    queryKey: ["sandbox", "fixtures", venue],
    queryFn: () => (venue ? getFixtures(venue) : Promise.resolve([])),
    enabled: !!venue,
    staleTime: 5000,
  })
}
