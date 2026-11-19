import type { PropType } from 'vue'
import type { AccountTypeView, ProxyTicketSource } from '@sub2api/host'
import type { NativeAccountFormProps } from '@sub2api/ui'

/** The props the account editor passes to a native account form (CONTRACTS §23.5). */
export const accountFormProps = {
  modelValue: { type: Object as PropType<Record<string, unknown>>, default: () => ({}) },
  mode: { type: String as PropType<'create' | 'edit'>, default: 'create' },
  account: { type: Object as PropType<NativeAccountFormProps['account']>, default: null },
  accountType: { type: Object as PropType<AccountTypeView | null>, default: null },
  errors: { type: Object as PropType<Record<string, string>>, default: undefined },
  proxyTicket: { type: Function as PropType<ProxyTicketSource>, default: undefined }
} as const
