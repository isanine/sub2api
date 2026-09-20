<template>
  <BaseDialog
    :show="show"
    :title="t('admin.accounts.recycleBin.title')"
    width="wide"
    @close="handleClose"
  >
    <div class="space-y-4">
      <p class="text-xs text-gray-500 dark:text-gray-400">
        {{ t('admin.accounts.recycleBin.desc') }}
      </p>

      <div v-if="loading" class="flex items-center justify-center py-12">
        <div class="h-8 w-8 animate-spin rounded-full border-2 border-primary-500 border-t-transparent"></div>
      </div>

      <template v-else>
        <div v-if="!entries.length" class="py-12 text-center text-sm text-gray-500 dark:text-gray-400">
          {{ t('admin.accounts.recycleBin.empty') }}
        </div>

        <div v-else class="max-h-[55vh] overflow-y-auto">
          <table class="w-full text-sm">
            <thead class="sticky top-0 bg-gray-50 text-left text-xs uppercase tracking-wide text-gray-500 dark:bg-dark-800 dark:text-gray-400">
              <tr>
                <th class="px-3 py-2">{{ t('admin.accounts.recycleBin.name') }}</th>
                <th class="px-3 py-2">{{ t('admin.accounts.recycleBin.platform') }}</th>
                <th class="px-3 py-2">{{ t('admin.accounts.recycleBin.type') }}</th>
                <th class="px-3 py-2">{{ t('admin.accounts.recycleBin.deletedAt') }}</th>
                <th class="px-3 py-2">{{ t('admin.accounts.recycleBin.deletedBy') }}</th>
                <th class="px-3 py-2 text-right">{{ t('admin.accounts.recycleBin.actions') }}</th>
              </tr>
            </thead>
            <tbody class="divide-y divide-gray-100 dark:divide-dark-700">
              <tr v-for="entry in entries" :key="entry.id" class="hover:bg-gray-50 dark:hover:bg-dark-800/60">
                <td class="px-3 py-2">
                  <div class="font-medium text-gray-900 dark:text-gray-100">{{ entry.name }}</div>
                  <div class="text-xs text-gray-400">#{{ entry.account_id }}</div>
                </td>
                <td class="px-3 py-2 text-gray-600 dark:text-gray-300">{{ entry.platform }}</td>
                <td class="px-3 py-2 text-gray-600 dark:text-gray-300">{{ entry.type }}</td>
                <td class="px-3 py-2 text-xs text-gray-500 dark:text-gray-400">{{ formatTime(entry.deleted_at) }}</td>
                <td class="px-3 py-2 text-xs text-gray-500 dark:text-gray-400">{{ entry.deleted_by_email || '-' }}</td>
                <td class="px-3 py-2 text-right">
                  <div class="flex justify-end gap-2">
                    <button
                      class="rounded-md bg-primary-50 px-2.5 py-1 text-xs font-medium text-primary-700 transition-colors hover:bg-primary-100 dark:bg-primary-900/30 dark:text-primary-300 dark:hover:bg-primary-900/50"
                      :disabled="busyId === entry.id"
                      @click="handleRestore(entry)"
                    >
                      {{ busyId === entry.id ? '…' : t('admin.accounts.recycleBin.restore') }}
                    </button>
                    <button
                      class="rounded-md bg-red-50 px-2.5 py-1 text-xs font-medium text-red-600 transition-colors hover:bg-red-100 dark:bg-red-900/30 dark:text-red-400 dark:hover:bg-red-900/50"
                      :disabled="busyId === entry.id"
                      @click="handlePurge(entry)"
                    >
                      {{ t('admin.accounts.recycleBin.purge') }}
                    </button>
                  </div>
                </td>
              </tr>
            </tbody>
          </table>
        </div>
      </template>
    </div>

    <ConfirmDialog
      :show="showPurgeDialog"
      :title="t('admin.accounts.recycleBin.purge')"
      :message="t('admin.accounts.recycleBin.purgeConfirm', { name: purgingEntry?.name })"
      :confirm-text="t('admin.accounts.recycleBin.purge')"
      :cancel-text="t('common.cancel')"
      :danger="true"
      @confirm="confirmPurge"
      @cancel="showPurgeDialog = false"
    />
  </BaseDialog>
</template>

<script setup lang="ts">
import { ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import BaseDialog from '@/components/common/BaseDialog.vue'
import ConfirmDialog from '@/components/common/ConfirmDialog.vue'
import { adminAPI } from '@/api/admin'
import type { AccountRecycleBinEntry } from '@/api/admin/accounts'

const props = defineProps<{ show: boolean }>()
const emit = defineEmits<{ (e: 'close'): void; (e: 'restored'): void }>()

const { t, locale } = useI18n()
const loading = ref(false)
const entries = ref<AccountRecycleBinEntry[]>([])
const busyId = ref<number | null>(null)
const showPurgeDialog = ref(false)
const purgingEntry = ref<AccountRecycleBinEntry | null>(null)

async function load() {
  loading.value = true
  try {
    entries.value = await adminAPI.accounts.listRecycleBin()
  } catch (error) {
    console.error('Failed to load recycle bin:', error)
    entries.value = []
  } finally {
    loading.value = false
  }
}

watch(
  () => props.show,
  (v) => {
    if (v) load()
  },
)

function handleClose() {
  if (loading.value) return
  emit('close')
}

async function handleRestore(entry: AccountRecycleBinEntry) {
  busyId.value = entry.id
  try {
    await adminAPI.accounts.restoreFromRecycleBin(entry.id)
    entries.value = entries.value.filter((e) => e.id !== entry.id)
    emit('restored')
  } catch (error) {
    console.error('Failed to restore account:', error)
  } finally {
    busyId.value = null
  }
}

function handlePurge(entry: AccountRecycleBinEntry) {
  purgingEntry.value = entry
  showPurgeDialog.value = true
}

async function confirmPurge() {
  if (!purgingEntry.value) return
  busyId.value = purgingEntry.value.id
  try {
    await adminAPI.accounts.purgeRecycleBinEntry(purgingEntry.value.id)
    entries.value = entries.value.filter((e) => e.id !== purgingEntry.value?.id)
    showPurgeDialog.value = false
    purgingEntry.value = null
  } catch (error) {
    console.error('Failed to purge recycle bin entry:', error)
  } finally {
    busyId.value = null
  }
}

function formatTime(value: string) {
  const d = new Date(value)
  if (Number.isNaN(d.getTime())) return value
  return d.toLocaleString(locale.value === 'zh' ? 'zh-CN' : 'en-US', { hour12: false })
}
</script>
