import { clsx, type ClassValue } from "clsx";
import { twMerge } from "tailwind-merge";

// cn merges conditional class lists (clsx) and de-duplicates conflicting
// Tailwind utilities (tailwind-merge) so component-level defaults can be
// overridden by a caller's className without specificity surprises.
export function cn(...inputs: ClassValue[]): string {
  return twMerge(clsx(inputs));
}
