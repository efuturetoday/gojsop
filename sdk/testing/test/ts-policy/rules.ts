export const isLatest = (image?: string): boolean => image?.endsWith(":latest") ?? false;

export function refuse(image: string): never {
  throw new Error(`refused ${image}`);
}
